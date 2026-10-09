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
	var binding Binding
	err := errNoBinding
	for i, paths := range s.paths {
		binding, err = BindingFrom(paths, ctx.Body)
		if errors.Is(err, errNoBinding) {
			continue
		}
		// A later pair may read a container other capabilities share -- a
		// support request's channels -- so a payload it cannot resolve is
		// refused only when it names this step's own work.
		if err != nil && i > 0 && !s.namesServed(paths, ctx.Body) {
			return nil
		}
		break
	}
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

// namesServed reports whether any provider and type the paths reach pair into
// a binding this step serves.
func (s *Step) namesServed(paths Paths, body []byte) bool {
	var payload any
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
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
	// What the reply means may be a refusal -- nothing on file -- which the
	// response half says under _error. The provider's body is not logged: it can
	// echo the farmer's own details, and redaction only covers credentials.
	if err := refusalIn(becknResponse); err != nil {
		log.Warnf(ctx, "%s refused the request: %v", plan.BindingKey, err)
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
	// Refused before the provider is called, so a bad request costs no call.
	if err := refusalIn(mapped); err != nil {
		return nil, err
	}
	if len(mapped) == 0 {
		log.Debugf(ctx, "the request half of %s produced nothing; sending an empty request", call.Mappings)
	}
	return mapped, nil
}

// refusalIn returns the error a mapping half asks for under the reserved
// _error field -- {status, code, message, path}, status absent meaning 400 --
// or nil when the document carries none. path is the JSONPath of the field at
// fault, returned as details.path. 202 is an answer with nothing in it, so it
// is an ACK carrying the reason.
func refusalIn(mapped []byte) error {
	// Checked as text first: every answer passes through here, and almost
	// none carries the field, so most are spared a second decode.
	if !bytes.Contains(mapped, []byte(`"_error"`)) {
		return nil
	}
	var document map[string]json.RawMessage
	if json.Unmarshal(mapped, &document) != nil || document["_error"] == nil {
		return nil
	}
	var r struct {
		Status  int    `json:"status"`
		Code    string `json:"code"`
		Message string `json:"message"`
		Path    string `json:"path"`
	}
	if json.Unmarshal(document["_error"], &r) != nil || r.Message == "" {
		return errors.New("a mapping refused the call with an unreadable _error; it needs status, code and message")
	}
	switch r.Status {
	case 0, http.StatusBadRequest:
		// The one 400 that carries a path on the wire: CodedErr drops details.
		return &model.SchemaValidationErr{Errors: []model.Error{*model.NewCodedErrorWithCause(r.Code, r.Message, r.Path, nil)}}
	case http.StatusAccepted:
		return model.NewAckNoCallbackErr(model.StatusACK, &model.Error{Code: r.Code, Message: r.Message})
	default:
		return model.NewCodedErr(r.Status, r.Code, errors.New(r.Message))
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
