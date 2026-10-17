// The request pipeline: recognise the capability, resolve its call plan,
// translate out, call, translate back.
package common

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	// An earlier step already routed this. The binding would still match, so
	// without this we would call the upstream for an answer about to be discarded.
	if ctx.Route != nil {
		return nil
	}

	// Is this ours to answer at all? Asked BEFORE the binding -- see routeElsewhere.
	routed, err := s.routeElsewhere(ctx)
	if err != nil || routed {
		return err
	}

	binding, err := s.bindingFor(ctx.Body)
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

// bindingFor derives the binding this step should answer for, trying the
// configured path pairs in the order they were written.
//
// A pair that resolves a binding some other step serves does not end the
// search. Several steps share one pipeline, and a later pair may read a
// container holding more than one step's entry, so this step keeps looking for
// its own work. When none of the pairs names it, the first binding found is
// what comes back, so the pass-through log says what the payload was asking for.
func (s *Step) bindingFor(body []byte) (Binding, error) {
	payload, err := decodePayload(body)
	if err != nil {
		return Binding{}, err
	}
	var first Binding
	found := false
	for i, paths := range s.paths {
		binding, err := bindingIn(paths, payload)
		switch {
		case errors.Is(err, errNoBinding):
			continue
		case err != nil:
			// The first pair is this step's own, so its refusal always stands.
			// A later pair reads a shared container, such as the support
			// channels, where another step's entry can make the payload name
			// several bindings without it being this caller's fault.
			if i > 0 && !s.namesServed(paths, payload) {
				continue
			}
			return Binding{}, err
		}
		if s.serves(binding.Key()) {
			return binding, nil
		}
		if !found {
			first, found = binding, true
		}
	}
	if found {
		return first, nil
	}
	return Binding{}, errNoBinding
}

// namesServed reports whether the payload names a binding this step serves.
//
// Takes the decoded payload rather than the body: it is reached only after
// bindingIn has already refused one, and re-reading the bytes to say which
// binding was named would parse the same document twice.
func (s *Step) namesServed(paths Paths, payload any) bool {
	for _, provider := range ValuesAt(payload, paths.ProviderID) {
		for _, capability := range ValuesAt(payload, paths.CapabilityCode) {
			if s.serves(Binding{ParticipantID: provider, CapabilityCode: capability}.Key()) {
				return true
			}
		}
	}
	return false
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
	provider := providerIDFrom(plan.BindingKey)
	auth, configured := s.auth[provider]
	if !configured {
		return fmt.Errorf("no credential is configured for %s", plan.BindingKey)
	}

	// The binding's host, unless another binding serves this action.
	// auth stays the originating provider's credential, not the host's.
	base := plan.BaseURL
	if key := forBinding(s.config.HostedElsewhere, plan.BindingKey)[action]; key != "" {
		if base, err = HostOf(ctx, s.registry, key); err != nil {
			return err
		}
	}

	// The provider's envelope, if any: seal once before the retry loop, open
	// once after it.
	var open Opener
	if envelope := forBinding(s.config.Envelopes, plan.BindingKey); envelope != nil {
		exchange := Exchange{Action: action, Beckn: beckn, Plan: plan, Registry: s.registry}
		if upstreamRequest, open, err = envelope.Seal(ctx, exchange, upstreamRequest); err != nil {
			return err
		}
	}

	upstreamResponse, err := s.call(ctx, auth, base, call, upstreamRequest)
	if err != nil {
		return err
	}

	if open != nil {
		if upstreamResponse, err = open(ctx, upstreamResponse); err != nil {
			return err
		}
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
	// The provider body is not logged: it can carry the farmer's details.
	// Its message is, so it is stripped of both our credentials and whatever
	// of the request it quoted back -- a refusal naming the mobile it could
	// not find is the ordinary shape of one.
	if cause, err := refusalIn(becknResponse); err != nil {
		log.Warnf(ctx, "%s refused the request: %v (provider said: %q)",
			plan.BindingKey, err, s.redactString(redactSent(cause, upstreamRequest)))
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
	if _, err := refusalIn(mapped); err != nil {
		return nil, err
	}
	if len(mapped) == 0 {
		log.Debugf(ctx, "the request half of %s produced nothing; sending an empty request", call.Mappings)
	}
	return mapped, nil
}

// refusalIn returns the error a mapping asks for under the reserved _error
// field {status, code, message, path, cause}, or nil if there is none. Status
// 0 or 400 is a 400 with details.path. cause is the provider's own wording,
// returned for the log and never sent to the caller.
func refusalIn(mapped []byte) (string, error) {
	// Cheap text check first: almost no answer carries the field.
	if !bytes.Contains(mapped, []byte(`"_error"`)) {
		return "", nil
	}
	// Only _error is held raw. Decoding the whole document into
	// map[string]json.RawMessage would copy every other top-level value's
	// bytes, and an answer carrying a case or an application is most of them.
	var document struct {
		Error json.RawMessage `json:"_error"`
	}
	if json.Unmarshal(mapped, &document) != nil || len(document.Error) == 0 ||
		string(document.Error) == "null" {
		return "", nil
	}
	// Decoded separately so a malformed _error is told apart from an absent
	// one: a single pass would fail whole and read as "no refusal here".
	var r struct {
		Status  int    `json:"status"`
		Code    string `json:"code"`
		Message string `json:"message"`
		Path    string `json:"path"`
		Cause   string `json:"cause"`
	}
	if json.Unmarshal(document.Error, &r) != nil || r.Message == "" {
		return "", errors.New("a mapping refused the call with an unreadable _error; it needs status, code and message")
	}
	switch r.Status {
	case 0, http.StatusBadRequest:
		// CodedErr drops details, so the path needs SchemaValidationErr.
		return r.Cause, &model.SchemaValidationErr{Errors: []model.Error{*model.NewCodedErrorWithCause(r.Code, r.Message, r.Path, nil)}}
	default:
		return r.Cause, model.NewCodedErr(r.Status, r.Code, errors.New(r.Message))
	}
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

// forBinding is the setting for bindingKey: its own, else its provider's.
func forBinding[T any](byKey map[string]T, bindingKey string) T {
	if v, ok := byKey[bindingKey]; ok {
		return v
	}
	return byKey[providerIDFrom(bindingKey)]
}

// routeElsewhere sends a request addressed to another network to that network.
// Before the binding check: a binding key names the upstream, not the network.
func (s *Step) routeElsewhere(ctx *model.StepContext) (bool, error) {
	becknContext, err := contextOf(ctx.Body)
	if err != nil {
		return false, err
	}

	// Refused rather than assumed. Guessing "ours" is what answers another
	// network's request with our data.
	receiver := contextValue(becknContext, "bpp_id", "bppId", "receiverId")
	if receiver == "" {
		return false, model.NewBadReqErr("", fmt.Errorf(
			"the request names no receiver, so there is nothing to decide by: "+
				"set context.receiverId to the participant that should answer"))
	}

	if ctx.OwnSubscriberID == "" {
		return false, fmt.Errorf(
			"the request is addressed to %s, but this module has no subscriberId to compare it against",
			receiver)
	}
	if strings.EqualFold(receiver, ctx.OwnSubscriberID) {
		return false, nil // ours: carry on into the binding check
	}

	if err := s.agreedToReadFrom(ctx, receiver); err != nil {
		return false, err
	}

	target, err := destinationOf(becknContext, receiver, ctx.OwnSubscriberID)
	if err != nil {
		return false, err
	}

	// The network signs, not the participant the request names. Without this
	// sign would look up the other network's keyset and fail.
	ctx.SubID = ctx.OwnSubscriberID
	ctx.Route = &model.Route{TargetType: "url", URL: target}

	log.Infof(ctx, "%s is not %s; sending it to %s", receiver, ctx.OwnSubscriberID, target)
	return true, nil
}

// agreedToReadFrom refuses a receiver we did not admit as a source. Without it
// a field in the request decides where a signed call of ours goes.
func (s *Step) agreedToReadFrom(ctx *model.StepContext, receiver string) error {
	if s.networks == nil {
		return fmt.Errorf(
			"the request is for %s, but this registry cannot list the networks we admitted",
			receiver)
	}

	admitted, err := s.networks.AdmittedNetworks(ctx)
	if err != nil {
		return fmt.Errorf("could not check whether %s is a network we admitted: %w", receiver, err)
	}

	for _, network := range admitted {
		if !strings.EqualFold(network.ID, receiver) {
			continue
		}
		if !network.IsSource() {
			return model.NewBadReqErr("", fmt.Errorf(
				"we agreed %v with %s, which does not include reading from them",
				network.Directions(), receiver))
		}
		log.Infof(ctx, "%s is admitted, agreed %v; forwarding", receiver, network.Directions())
		return nil
	}

	return model.NewNotFoundErr("", fmt.Errorf(
		"%s is not a network we admitted, so this is not ours to send on", receiver))
}

// destinationOf builds the URL a request for another participant is sent to.
// The address is context.bppUri, so the caller names where we send it.
func destinationOf(becknContext map[string]any, receiver, us string) (*url.URL, error) {
	raw := contextValue(becknContext, "bpp_uri", "bppUri", "receiverUri")
	if raw == "" {
		return nil, model.NewBadReqErr("", fmt.Errorf(
			"the request is for %s rather than %s, but names no context.bppUri, "+
				"so there is no address to send it to", receiver, us))
	}

	target, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || target.Host == "" {
		return nil, model.NewBadReqErr("SCH_INVALID_FORMAT", fmt.Errorf(
			"context.bppUri %q is not a URL this request can be sent to", raw))
	}

	// The proxy replaces the request URL outright, so a bare bppUri would
	// arrive at the other network's mount with no action left on it.
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

// contextOf reads the request's Beckn context. An unreadable body is reported
// as that, not as "names no receiver" -- this is the first step to see it.
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

// contextValue reads the first of the named context fields carrying a value,
// snake_case then camelCase then Beckn v2, the order the adapter uses.
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
