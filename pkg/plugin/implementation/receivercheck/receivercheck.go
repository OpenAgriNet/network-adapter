// Package receivercheck refuses a request that names somebody else as the
// participant who should answer it.
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
package receivercheck

import (
	"encoding/json"
	"fmt"
	"strings"

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
		return nil, fmt.Errorf("checkReceiver: subscriberId is required; " +
			"without it this module cannot tell a request for itself from one for a peer")
	}
	return &Step{subscriberID: subscriberID}, nil
}

// Run passes a request addressed to this module through untouched, and refuses
// every other one.
//
// Doing nothing on a match IS the convention the provider steps follow: a step
// recognises its own work and otherwise lets the chain continue. So a request
// for us takes exactly the path it took before this step existed.
func (s *Step) Run(ctx *model.StepContext) error {
	receiver := receiverOf(ctx.Body)

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

	// 404 rather than 403, and deliberately: this is not a permission the
	// caller might be granted, it is the wrong address. It also matches the
	// handler's own "serves no capability matching the request", so a caller
	// that reached the wrong network sees one shape of answer either way.
	//
	// The message names both ends because the fix is the caller's: a select
	// carries context.bppUri from the catalog it chose, and arriving here with
	// somebody else's receiver means those two disagree.
	return model.NewNotFoundErr("", fmt.Errorf(
		"this request is addressed to %s, but this is %s; "+
			"route it to the participant named in the catalog's bppUri",
		receiver, s.subscriberID))
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
