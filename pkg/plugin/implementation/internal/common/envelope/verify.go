package envelope

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
)

// Hooks are what the capability knows and this package does not: where its
// payload carries the challenge, and what its provider's "yes" looks like.
type Hooks struct {
	// Challenge builds the verify request from the payload, or a coded
	// refusal. Needed only by a provider that verifies.
	Challenge func(exchange common.Exchange) (map[string]any, error)

	// Succeeded reports whether an opened answer says yes. It must fail
	// closed: anything it cannot read is a no.
	Succeeded func(plain []byte) bool
}

// verify checks the caller's challenge before the guarded call. The challenge
// goes only here, never to the call, the log or the reply.
type verify struct {
	provider string
	path     string
	actions  map[string]bool
	codec    *cbcCodec
	// bindingKey names the OTP service's participant; its host is read per call.
	bindingKey string
	hooks      Hooks
	client     *http.Client
	budget     model.ActionPlan // the verify's own, not the guarded action's
}

// check verifies the challenge. Unreachable: 502. Not a yes: 400
// BIZ_GENERIC_ERROR (401 means a bad Beckn signature).
func (v *verify) check(ctx context.Context, exchange common.Exchange) error {
	fields, err := v.hooks.Challenge(exchange)
	if err != nil {
		return err
	}
	base, err := common.HostOf(ctx, exchange.Registry, v.bindingKey)
	if err != nil {
		return err
	}
	body, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	sealed, open, err := v.codec.seal(ctx, body)
	if err != nil {
		return err
	}

	answer, err := postSealed(ctx, v.client, v.provider, "OTP verification", base, v.path, v.budget, sealed)
	if err != nil {
		return err
	}
	plain, err := open(ctx, answer)
	if err != nil {
		return err
	}
	if !v.hooks.Succeeded(plain) {
		// The provider's message is never logged or returned.
		log.Warnf(ctx, "%s refused the OTP", v.provider)
		return model.NewCodedErr(http.StatusBadRequest, "BIZ_GENERIC_ERROR",
			errors.New("the OTP is wrong or has expired"))
	}
	return nil
}
