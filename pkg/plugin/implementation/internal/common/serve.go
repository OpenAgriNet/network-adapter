// The request pipeline: recognise the capability, resolve its call plan,
// translate out, call, translate back.
package common

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

// Run serves the request when it is this step's capability, and does nothing
// when it is not.
//
// Doing nothing IS the dispatch: several steps sit in one pipeline and each
// recognises its own work, so adding a provider is one config entry rather than
// a routing-table change.
func (s *Step) Run(ctx *model.StepContext) error {
	// An earlier step has already decided this request belongs somewhere else.
	// A step list runs every step regardless, so without this the binding would
	// still match -- a binding key names the upstream provider, not the network
	// -- and we would call that upstream for an answer the proxy is about to
	// discard, and answer in somebody else's name while doing it.
	if ctx.Route != nil {
		return nil
	}

	// Is this ours to answer at all? Asked BEFORE the binding, and that
	// ordering is the whole point -- see routeElsewhere.
	routed, err := s.routeElsewhere(ctx)
	if err != nil || routed {
		return err
	}

	binding, err := BindingFrom(s.paths, ctx.Body)
	if errors.Is(err, errNoBinding) {
		return nil
	}
	if err != nil {
		// Everything BindingFrom refuses is about the payload -- unreadable
		// JSON, or more than one call named. Unclassified it becomes a 500,
		// which blames this adapter and hides the reason from the caller.
		return model.NewBadReqErr("", err)
	}
	if !s.serves(binding.Key()) {
		log.Debugf(ctx, "%s is not one of this step's capabilities, passing through", binding.Key())
		return nil
	}

	plan, err := s.registry.ProviderRecord(ctx, binding.Key())
	if err != nil {
		// "No such binding" is the caller naming something absent, so 404. A
		// registry that could not be consulted is this adapter failing, so it
		// stays a 500.
		if errors.Is(err, definition.ErrProviderRecordNotFound) {
			// %w, not %v: the sentinel must stay unwrappable or errors.Is
			// stops matching.
			return model.NewNotFoundErr("", fmt.Errorf(
				"the registry publishes no active binding for %s: %w", binding.Key(), err))
		}
		return fmt.Errorf("no call plan for %s: %w", binding.Key(), err)
	}

	return s.serve(ctx, plan)
}

// resolve runs this capability's prerequisite work, returning what the mapping
// reads under _local.
//
// Empty rather than nil: a mapping referring to _local when nothing resolved
// should read a missing field, not fail.
func (s *Step) resolve(ctx context.Context, bindingKey string, beckn any) (map[string]any, error) {
	prerequisite, needed := s.prerequisites[bindingKey]
	if !needed {
		return map[string]any{}, nil
	}
	local, err := prerequisite(ctx, beckn)
	if err != nil {
		return nil, fmt.Errorf("%s could not resolve what it needs before the call: %w", bindingKey, err)
	}
	if local == nil {
		return map[string]any{}, nil
	}
	return local, nil
}

// serves reports whether a binding key is one this step answers to.
//
// A slice, not a set: a step serves a handful of capabilities, so the scan is
// cheaper than a map and config order survives into New's startup log.
func (s *Step) serves(key string) bool {
	return slices.Contains(s.config.BindingKeys, key)
}

// serve runs the exchange this step exists for: resolve, map out, call, map back.
func (s *Step) serve(ctx *model.StepContext, plan *model.ProviderRecord) error {
	action := extractAction(ctx.Body)
	call, served := plan.Actions[action]
	if !served {
		// No endpoint published for this action. Refused here rather than
		// after calling whichever endpoint was on the record, and the error
		// names what IS served so a registry mistake is a one-line fix.
		return model.NewBadReqErr("", fmt.Errorf(
			"%s does not serve action %q; it serves %s",
			plan.BindingKey, action, strings.Join(plan.ServedActions(), ", ")))
	}

	beckn, err := decodeBody(ctx.Body)
	if err != nil {
		return err
	}

	// The mapping declares what a payload must satisfy, not this step: a
	// different rule is a different mapping file, not a rebuild.
	if err := s.mapper.Verify(ctx, call.Mappings, map[string]any{"beckn": beckn}); err != nil {
		return err
	}

	// Whatever the payload does not carry. Empty for most capabilities.
	local, err := s.resolve(ctx, plan.BindingKey, beckn)
	if err != nil {
		return err
	}

	upstreamRequest, err := s.buildRequest(ctx, call, beckn, local)
	if err != nil {
		return err
	}

	// Resolved from the binding key, so a step serving several providers
	// authenticates each as itself. Startup guarantees a profile per served
	// provider; this guards a record arriving for an undeclared key.
	auth, configured := s.auth[providerIDFrom(plan.BindingKey)]
	if !configured {
		return fmt.Errorf("no credential is configured for %s", plan.BindingKey)
	}

	upstreamResponse, err := s.call(ctx, auth, plan.BaseURL, call, upstreamRequest)
	if err != nil {
		return err
	}

	answer, err := decodeBody(upstreamResponse)
	if err != nil {
		return fmt.Errorf("provider answered with something that is not JSON: %w", err)
	}

	// The same file's other half. It is handed what each party sent, plus
	// whatever prerequisites resolved, under _local.
	becknResponse, err := s.mapper.Transform(ctx, call.Mappings, definition.DirectionResponse, map[string]any{
		"beckn":    beckn,
		"_local":   local,
		"response": answer,
	})
	if err != nil {
		return err
	}
	if len(becknResponse) == 0 {
		// No response half, or its transform matched nothing. Either way there
		// is no Beckn response, and returning the provider's own shape would
		// be worse than failing.
		return fmt.Errorf("the response half of %s produced nothing, so %s cannot be answered",
			call.Mappings, plan.BindingKey)
	}

	ctx.ResponseBody = becknResponse
	log.Infof(ctx, "served %s in %d bytes", plan.BindingKey, len(becknResponse))
	return nil
}

// buildRequest produces what the provider is sent.
//
// Whatever the mapping produces IS the request: a body for a method that takes
// one, query parameters otherwise. Nothing is substituted when it produces
// nothing, so which payload fields reach a provider is a mapping edit rather
// than a rebuild.
func (s *Step) buildRequest(ctx context.Context, call model.ActionPlan, beckn any, local map[string]any) ([]byte, error) {
	mapped, err := s.mapper.Transform(ctx, call.Mappings, definition.DirectionRequest, map[string]any{
		"beckn":  beckn,
		"_local": local,
	})
	if err != nil {
		return nil, err
	}
	if len(mapped) == 0 {
		log.Debugf(ctx, "the request half of %s produced nothing; sending an empty request", call.Mappings)
	}
	return mapped, nil
}

// extractAction reads the Beckn action a request is for.
func extractAction(body []byte) string {
	var payload struct {
		Context struct {
			Action string `json:"action"`
		} `json:"context"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return payload.Context.Action
}

// decodeBody turns raw JSON into the generic value a mapping reads.
func decodeBody(body []byte) (any, error) {
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("could not read JSON: %w", err)
	}
	return decoded, nil
}

// routeElsewhere sends a request addressed to another network to that network,
// and reports whether it did.
//
// Here rather than in a step of its own, so that a deployment gets it by
// serving capabilities at all -- there is no list to leave it out of.
//
// BEFORE the binding check, and that ordering is the whole point. A binding key
// is "<providerParticipantId>|<capability>", so it names the UPSTREAM provider
// and not the network fronting it. Two networks that both front the same
// upstream therefore hold the SAME binding keys -- the ordinary case, not a
// misconfiguration -- and a select meant for one of them, arriving here, would
// match one of ours and be answered with OUR data under their name.
//
// The handler's own 404 does not catch that: it fires when NOTHING answered,
// and here something did. "Nobody here serves that" and "somebody here serves
// that, but it was not addressed to us" are different failures.
//
// It makes no HTTP call. Setting ctx.Route is how every handler-to-handler hop
// in this adapter is expressed, and the proxy that follows the step chain
// performs the call and returns the answer to the original caller.
func (s *Step) routeElsewhere(ctx *model.StepContext) (bool, error) {
	// Without an identity this cannot tell our own request from anyone else's,
	// and guessing "ours" is the failure this exists to prevent. The handler
	// supplies it from the module's own subscriberId, so an empty one means a
	// module that declared none.
	if s.config.SubscriberID == "" {
		return false, fmt.Errorf(
			"this module cannot tell whether a request is its own to answer: " +
				"it has no subscriberId")
	}

	becknContext, err := contextOf(ctx.Body)
	if err != nil {
		return false, err
	}

	receiver := contextValue(becknContext, "bpp_id", "bppId", "receiverId")
	if receiver == "" {
		// Refused rather than assumed. The whole decision rests on this field,
		// and guessing "ours" is what lets another network's request be
		// answered with our data.
		return false, model.NewBadReqErr("", fmt.Errorf(
			"the request names no receiver, so there is nothing to decide by: "+
				"set context.receiverId to the participant that should answer "+
				"(this module is %s)", s.config.SubscriberID))
	}
	if strings.EqualFold(receiver, s.config.SubscriberID) {
		return false, nil // ours: carry on into the binding check below
	}

	target, err := destinationOf(becknContext, receiver, s.config.SubscriberID)
	if err != nil {
		return false, err
	}

	// The network signs, not the participant the request names. Without this
	// the sign step would sign as the OTHER network: reqpreprocessor resolves
	// the module's own id from the body, which on this path is somebody else,
	// and the keyset lookup fails with "failed to get signing key".
	ctx.SubID = s.config.SubscriberID
	ctx.Route = &model.Route{TargetType: "url", URL: target}

	log.Debugf(ctx, "%s is not %s; sending it to %s",
		receiver, s.config.SubscriberID, target)
	return true, nil
}

// destinationOf builds the URL a request for another participant is sent to.
//
// The address is NOT configured anywhere. It is context.bppUri, which the
// consumer copied from the catalog it chose, so a catalog crawled from another
// network carries that network's own published address and nothing here needs
// to know who the peers are.
func destinationOf(becknContext map[string]any, receiver, us string) (*url.URL, error) {
	raw := contextValue(becknContext, "bpp_uri", "bppUri", "receiverUri")
	if raw == "" {
		return nil, model.NewBadReqErr("", fmt.Errorf(
			"the request is for %s rather than %s, but names no context.bppUri, "+
				"so there is no address to send it to", receiver, us))
	}

	target, err := url.Parse(raw)
	if err != nil || target.Host == "" {
		return nil, model.NewBadReqErr("SCH_INVALID_FORMAT", fmt.Errorf(
			"context.bppUri %q is not a URL this request can be sent to", raw))
	}

	// The action has to survive the hop: the proxy replaces the request URL
	// with the route's outright, so a bare bppUri would arrive at the other
	// network's mount point with no endpoint left on it.
	action := contextValue(becknContext, "action")
	if action == "" {
		return nil, model.NewBadReqErr("", fmt.Errorf(
			"the request is for %s rather than %s, but names no context.action, "+
				"so there is no endpoint to send it to", receiver, us))
	}
	if target.Path == "" {
		target.Path = "/"
	}
	target.Path = path.Join(target.Path, action)
	return target, nil
}

// contextOf reads the request's Beckn context.
//
// A body that is not JSON is reported as unreadable rather than as "names no
// receiver": this check runs before the binding, so it is the first thing to
// see a malformed body and the first chance to say what is actually wrong.
func contextOf(body []byte) (map[string]any, error) {
	var payload struct {
		Context map[string]any `json:"context"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, model.NewBadReqErr("SCH_INVALID_FORMAT",
			fmt.Errorf("payload could not be read: %w", err))
	}
	return payload.Context, nil
}

// contextValue reads the first of the named context fields that carries a value.
//
// The keys are passed as an alias chain -- snake_case first, then camelCase,
// then the Beckn v2 name -- which is the order the rest of the adapter uses, so
// a payload in any of the three spellings is understood identically.
func contextValue(becknContext map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := becknContext[key].(string); ok {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}
