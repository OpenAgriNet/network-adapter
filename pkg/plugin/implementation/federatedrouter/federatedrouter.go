// Package federatedrouter sends an action meant for another network to that
// network, instead of failing to find its provider here.
//
// The gap it fills: a provider we do not serve falls through every provider
// step -- each returns nil for a binding it does not recognise -- and nothing
// answers it. Before federation that was correct, because the provider really
// was absent. Once networks federate it is not: the provider may be real and
// simply belong to a peer.
package federatedrouter

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
)

// Config names the two things this step cannot work out for itself.
type Config struct {
	// SubscriberID is what THIS module registered as -- the participant id a
	// request names when it wants this adapter to answer.
	//
	// It must come from config and never from the request. The adapter's
	// "own id" on the step context is resolved FROM THE BODY by
	// reqpreprocessor (ResolveSubscriberID reads bppId/receiverId), so
	// comparing the receiver against it would compare the field with itself:
	// always equal, every request local, and the branch below would never
	// fire while appearing to work.
	SubscriberID string

	// Forward is where a request for another network is sent: this
	// deployment's outbound module, which signs as the network and routes to
	// context.bppUri.
	//
	// A URL rather than a module name because the two may be different
	// processes. In a consolidated deployment it points back at this same
	// adapter on another path, and nothing else changes.
	Forward *url.URL
}

// Step decides whether a request is ours to answer.
type Step struct{ cfg Config }

// New builds the step from its YAML config.
func New(raw map[string]string) (*Step, error) {
	subscriberID := strings.TrimSpace(raw["subscriberId"])
	if subscriberID == "" {
		return nil, fmt.Errorf("federated: subscriberId is required; " +
			"without it this module cannot tell a request for itself from one for a peer")
	}

	forward := strings.TrimSpace(raw["forwardUrl"])
	if forward == "" {
		return nil, fmt.Errorf("federated: forwardUrl is required; " +
			"it is where a request for another network is sent to be signed and routed")
	}
	target, err := url.Parse(forward)
	if err != nil {
		return nil, fmt.Errorf("federated: forwardUrl %q is not a URL: %w", forward, err)
	}

	return &Step{cfg: Config{SubscriberID: subscriberID, Forward: target}}, nil
}

// Run routes the request onward when another participant should answer it, and
// does nothing when this module should.
//
// Doing nothing IS the dispatch, the same convention the provider steps follow:
// a step recognises its own work and otherwise lets the chain continue. So a
// request for us takes exactly the path it took before this step existed.
//
// It makes no HTTP call. Setting ctx.Route is how every handler-to-handler hop
// in this adapter is expressed -- addRoute does only this -- and the proxy that
// follows the step chain performs the call and returns the answer.
func (s *Step) Run(ctx *model.StepContext) error {
	receiver := receiverOf(ctx.Body)
	if receiver == "" {
		// Refused rather than assumed. The whole decision rests on this field,
		// and guessing in either direction is worse than saying so: guessing
		// "ours" hides a cross-network request as a local 404, and guessing
		// "theirs" sends a local request out of the network.
		return model.NewBadReqErr("", fmt.Errorf(
			"the request names no receiver, so there is nothing to decide by: "+
				"set context.receiverId to the participant that should answer (this module is %s)",
			s.cfg.SubscriberID))
	}
	if strings.EqualFold(receiver, s.cfg.SubscriberID) {
		return nil
	}

	// The action has to survive the hop. The proxy replaces the request URL
	// with the route's outright, so forwarding to the bare module path would
	// arrive with nothing left after the mount point -- and the outbound
	// module's router, which matches on the endpoint, would find none.
	action := actionOf(ctx.Body)
	if action == "" {
		return model.NewBadReqErr("", fmt.Errorf(
			"the request is for %s rather than %s, but names no context.action, "+
				"so there is no endpoint to forward it to", receiver, s.cfg.SubscriberID))
	}

	target := *s.cfg.Forward
	target.Path = strings.TrimRight(target.Path, "/") + "/" + action

	log.Debugf(ctx, "federated: %s is not %s, forwarding to %s",
		receiver, s.cfg.SubscriberID, target.String())

	ctx.Route = &model.Route{TargetType: "url", URL: &target}
	return nil
}

// actionOf reads the Beckn action, which is the endpoint the outbound module
// routes on.
func actionOf(body []byte) string {
	var payload struct {
		Context struct {
			Action string `json:"action"`
		} `json:"context"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.Context.Action)
}

// receiverOf reads who the request expects to answer it.
//
// The same alias chain the rest of the adapter uses -- snake_case first, then
// camelCase, then the Beckn v2 name -- so a payload in any of the three
// spellings is understood identically.
func receiverOf(body []byte) string {
	var payload struct {
		Context map[string]any `json:"context"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	for _, key := range []string{"bpp_id", "bppId", "receiverId"} {
		if value, ok := payload.Context[key].(string); ok {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}
