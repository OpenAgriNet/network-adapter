// The request pipeline: recognise the capability, resolve its call plan,
// translate out, call, translate back.
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/capabilitybinding"
)

// Run serves the request when it is for this step's capability, and does
// nothing when it is not.
//
// Doing nothing is the dispatch mechanism: several provider steps sit in one
// pipeline and each recognises its own work, so adding a provider is one more
// entry rather than a change to a routing table.
func (s *Step) Run(ctx *model.StepContext) error {
	binding, err := capabilitybinding.From(s.paths, ctx.Body)
	if errors.Is(err, capabilitybinding.ErrNoBinding) {
		return nil
	}
	if err != nil {
		// Everything From refuses is a statement about the payload: unreadable
		// JSON, or a request naming more than one call. Unclassified it becomes
		// a 500, which says this adapter broke and leaves the reason in a log
		// the caller cannot read.
		return model.NewBadReqErr("", err)
	}
	if !s.serves(binding.Key()) {
		log.Debugf(ctx, "upstream: %s is not one of this step's capabilities, passing through", binding.Key())
		return nil
	}

	plan, err := s.registry.ProviderRecord(ctx, binding.Key())
	if err != nil {
		// A definite "no such binding" is the caller naming something that is
		// not there, so 404 -- the same reasoning the no-route path uses to
		// refuse an unrecognised capability rather than ACK it. A registry that
		// could not be consulted is different and stays a 500: unclassified,
		// because it is this adapter that failed.
		if errors.Is(err, definition.ErrProviderRecordNotFound) {
			// %w, not %v: the sentinel has to stay unwrappable, or anything
			// upstream testing errors.Is against it silently stops matching.
			return model.NewNotFoundErr("", fmt.Errorf(
				"upstream: the registry publishes no active binding for %s: %w", binding.Key(), err))
		}
		return fmt.Errorf("upstream: no call plan for %s: %w", binding.Key(), err)
	}

	return s.serve(ctx, plan)
}

// resolve runs whatever prerequisite work this capability needs, and returns the
// values for the mapping to read under _local.
//
// Empty rather than nil when there is nothing: a mapping referring to _local on
// a capability that resolves nothing should read a missing field, not fail.
func (s *Step) resolve(ctx context.Context, bindingKey string, beckn any) (map[string]any, error) {
	prerequisite, needed := s.prerequisites[bindingKey]
	if !needed {
		return map[string]any{}, nil
	}
	local, err := prerequisite(ctx, beckn)
	if err != nil {
		return nil, fmt.Errorf("upstream: %s could not resolve what it needs before the call: %w", bindingKey, err)
	}
	if local == nil {
		return map[string]any{}, nil
	}
	return local, nil
}

// serves reports whether a binding key is one this step answers to.
//
// A slice rather than a set: a step serves a handful of capabilities at most, so
// the scan costs less than the map would, and the config order is preserved in
// the log line above.
func (s *Step) serves(key string) bool {
	return slices.Contains(s.config.BindingKeys, key)
}

// serve runs the exchange this step exists for: resolve, map out, call, map back.
func (s *Step) serve(ctx *model.StepContext, plan *model.ProviderRecord) error {
	action := extractAction(ctx.Body)
	call, served := plan.Actions[action]
	if !served {
		// The capability publishes no endpoint for this action, so it does not
		// serve it. Refused here rather than after a call to whichever endpoint
		// happened to be on the record -- naming what it does serve turns a
		// registry mistake into a one-line fix.
		return model.NewBadReqErr("", fmt.Errorf(
			"upstream: %s does not serve action %q; it serves %s",
			plan.BindingKey, action, strings.Join(plan.ServedActions(), ", ")))
	}

	beckn, err := decodeBody(ctx.Body)
	if err != nil {
		return err
	}

	// What this provider requires of a payload is declared by its mapping, not
	// by this step. A capability with a different rule is a different mapping
	// file rather than a different build -- and the rule sits beside the
	// extraction it guards.
	if err := s.mapper.Verify(ctx, call.Mappings, map[string]any{"beckn": beckn}); err != nil {
		return err
	}

	// Whatever this capability needs that its payload does not carry. Empty for
	// most: the mapping reads the payload directly and needs nothing resolved.
	local, err := s.resolve(ctx, plan.BindingKey, beckn)
	if err != nil {
		return err
	}

	upstreamRequest, err := s.buildRequest(ctx, call, beckn, local)
	if err != nil {
		return err
	}

	// Which credential this provider takes. Resolved from the binding key, so
	// one step serving several providers authenticates each as its own.
	// Startup guarantees a profile per served provider; this guards the case
	// where a record arrives for a key the config never declared.
	auth, configured := s.auth[providerIDFrom(plan.BindingKey)]
	if !configured {
		return fmt.Errorf("upstream: no credential is configured for %s", plan.BindingKey)
	}

	upstreamResponse, err := s.call(ctx, auth, plan.BaseURL, call, upstreamRequest)
	if err != nil {
		return err
	}

	answer, err := decodeBody(upstreamResponse)
	if err != nil {
		return fmt.Errorf("upstream: provider answered with something that is not JSON: %w", err)
	}

	// The same mapping reference as the request, other half: one file carries
	// both directions for this action.
	//
	// The mapping is handed what each party sent, plus whatever prerequisites
	// resolved, under _local. Empty when there are none.
	becknResponse, err := s.mapper.Transform(ctx, call.Mappings, definition.DirectionResponse, map[string]any{
		"beckn":    beckn,
		"_local":   local,
		"response": answer,
	})
	if err != nil {
		return err
	}
	if len(becknResponse) == 0 {
		// Either the file has no response half, or its transform matched nothing
		// in this answer. Both leave no Beckn response to return, and returning
		// the provider's own shape instead would be worse than failing. The
		// message says what was observed rather than guessing which it was.
		return fmt.Errorf("upstream: the response half of %s produced nothing, so %s cannot be answered",
			call.Mappings, plan.BindingKey)
	}

	ctx.ResponseBody = becknResponse
	log.Infof(ctx, "upstream: served %s in %d bytes", plan.BindingKey, len(becknResponse))
	return nil
}

// buildRequest produces what the provider is sent.
//
// Whatever the mapping produces IS the request: a body for a method that takes
// one, query parameters for a method that does not. Nothing is substituted when
// it produces nothing, so an empty request half means an empty request.
//
// This step used to extract a point from the payload and fall back to sending
// that. It meant the choice of which payload fields reach the provider lived in
// Go, so adding a parameter -- a date range, say -- was a rebuild. Now it is a
// mapping edit and nothing else.
func (s *Step) buildRequest(ctx context.Context, call model.ActionPlan, beckn any, local map[string]any) ([]byte, error) {
	mapped, err := s.mapper.Transform(ctx, call.Mappings, definition.DirectionRequest, map[string]any{
		"beckn":  beckn,
		"_local": local,
	})
	if err != nil {
		return nil, err
	}
	if len(mapped) == 0 {
		log.Debugf(ctx, "upstream: the request half of %s produced nothing; sending an empty request", call.Mappings)
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
		return nil, fmt.Errorf("upstream: could not read JSON: %w", err)
	}
	return decoded, nil
}
