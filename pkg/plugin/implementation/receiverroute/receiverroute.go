// Package receiverroute sends a request to the participant it names, when that
// is not this one.
//
// The gap it fills is specific to federation. A binding key is
// "<providerParticipantId>|<capability>", so it names the UPSTREAM provider and
// not the network serving it. Two federated networks that both front the same
// upstream therefore hold the SAME binding keys -- which is the normal case,
// not a misconfiguration. A select meant for a peer, arriving here, matches one
// of our bindings and is answered with OUR data, under the peer's name.
//
// The handler's existing 404 does not catch it: that fires when no step
// answered at all, and here a step did. "Nobody here serves that" and "somebody
// here serves that, but it was not addressed to us" are different failures, and
// only the first was covered.
//
// The answer to the second is not a refusal. The request already carries where
// it belongs -- context.bppUri, which the consumer copied from the catalog it
// chose -- so this step sends it there. No configured forwarding address: the
// destination is published data and comes from the body.
package receiverroute

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
)

// Step answers one question: is this request addressed to this module?
type Step struct{ subscriberID string }

// New builds the step from its YAML config.
//
// subscriberID must come from config and never from the request. The adapter's
// "own id" on the step context is resolved FROM THE BODY by reqpreprocessor,
// which reads bppId/receiverId -- the very field being checked. Comparing the
// receiver against it would compare the field with itself: always equal, every
// request accepted, and the check would never fire while appearing to work.
func New(raw map[string]string) (*Step, error) {
	subscriberID := strings.TrimSpace(raw["subscriberId"])
	if subscriberID == "" {
		return nil, fmt.Errorf("routeByReceiver: subscriberId is required; " +
			"without it this module cannot tell a request for itself from one for a peer")
	}
	return &Step{subscriberID: subscriberID}, nil
}

// Run lets a request addressed to this module through untouched, and sends
// every other one to the participant it names.
//
// Doing nothing on a match IS the convention the provider steps follow: a step
// recognises its own work and otherwise lets the chain continue. So a request
// for us takes exactly the path it took before this step existed.
//
// It makes no HTTP call itself. Setting ctx.Route is how every handler-to-
// handler hop in this adapter is expressed -- addRoute does only this -- and
// the proxy that follows the step chain performs the call and returns the
// answer to the original caller.
func (s *Step) Run(ctx *model.StepContext) error {
	receiver := contextValue(ctx.Body, "bpp_id", "bppId", "receiverId")

	// Refused rather than assumed. The whole decision rests on this field, and
	// guessing is worse than saying so: guessing "ours" is what lets a peer's
	// request be answered with our data, which is the case this step exists for.
	if receiver == "" {
		return model.NewBadReqErr("", fmt.Errorf(
			"the request names no receiver, so there is nothing to decide by: "+
				"set context.receiverId to the participant that should answer (this module is %s)",
			s.subscriberID))
	}

	if strings.EqualFold(receiver, s.subscriberID) {
		return nil
	}

	target, err := s.destination(ctx.Body, receiver)
	if err != nil {
		return err
	}

	log.Debugf(ctx, "routeByReceiver: %s is not %s, forwarding to %s",
		receiver, s.subscriberID, target)

	// The network signs, not the participant the request names. Without this
	// the sign step would sign as the PEER: reqpreprocessor resolves the
	// module's own id from the body (role bpp reads receiverId), which on this
	// path is somebody else, and the keyset lookup fails with "failed to get
	// signing key".
	//
	// Set here rather than by dropping reqpreprocessor, which would also take
	// transaction_id off every log line on this module -- the handler sets
	// messageId itself but nothing else sets txnID.
	//
	// Only on this branch. A request for us keeps the id reqpreprocessor
	// resolved, which under one identity is this same string anyway.
	ctx.SubID = s.subscriberID

	ctx.Route = &model.Route{TargetType: "url", URL: target}
	return nil
}

// destination builds the URL a request for another participant is sent to.
//
// The address is NOT configured anywhere. It is context.bppUri, which the
// consumer copied from the catalog it chose, so a catalog crawled from a peer
// carries that peer's own published address and this step needs to know
// nothing about who the peers are.
func (s *Step) destination(body []byte, receiver string) (*url.URL, error) {
	raw := contextValue(body, "bpp_uri", "bppUri", "receiverUri")
	if raw == "" {
		return nil, model.NewBadReqErr("", fmt.Errorf(
			"the request is for %s rather than %s, but names no context.bppUri, "+
				"so there is no address to send it to", receiver, s.subscriberID))
	}

	target, err := url.Parse(raw)
	if err != nil || target.Host == "" {
		return nil, model.NewBadReqErr("SCH_INVALID_FORMAT", fmt.Errorf(
			"context.bppUri %q is not a URL this request can be sent to", raw))
	}

	// The action has to survive the hop: the proxy replaces the request URL
	// with the route's outright, so a bare bppUri would arrive at the peer's
	// mount point with no endpoint left on it.
	action := contextValue(body, "action")
	if action == "" {
		return nil, model.NewBadReqErr("", fmt.Errorf(
			"the request is for %s rather than %s, but names no context.action, "+
				"so there is no endpoint to send it to", receiver, s.subscriberID))
	}
	if target.Path == "" {
		target.Path = "/"
	}
	target.Path = path.Join(target.Path, action)
	return target, nil
}

// contextValue reads the first of the named context fields that carries a
// value.
//
// The keys are passed as an alias chain -- snake_case first, then camelCase,
// then the Beckn v2 name -- which is the order the rest of the adapter uses, so
// a payload in any of the three spellings is understood identically.
func contextValue(body []byte, keys ...string) string {
	var payload struct {
		Context map[string]any `json:"context"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	for _, key := range keys {
		if value, ok := payload.Context[key].(string); ok {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}
