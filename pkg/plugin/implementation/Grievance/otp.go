package Grievance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
)

// challengeMethod is the only accepted challenge.
const challengeMethod = "SMS_OTP"

// identityType is the OTP service's type for a registration number (enrolmentId).
const identityType = "Ben_id"

// applicantTypes maps an applicant slot to the OTP service's Types ("Aadhar" is
// the upstream's spelling).
var applicantTypes = map[string]string{
	"mobile":  "Mobile",
	"aadhaar": "Aadhar",
}

// otpVerify checks the farmer's OTP on the OTP service before the guarded call.
// The OTP goes only here: never to the guarded call, the log or the reply.
type otpVerify struct {
	provider   string
	path       string
	actions    map[string]bool
	hostAction string
	codec      *cbcCodec
	host       func(context.Context, common.Exchange) (string, error)
	client     *http.Client
}

func newOTPVerify(provider, path string, actions map[string]bool, hostAction string, codec *cbcCodec,
	host func(context.Context, common.Exchange) (string, error)) *otpVerify {
	return &otpVerify{provider: provider, path: path, actions: actions, hostAction: hostAction,
		codec: codec, host: host, client: &http.Client{}}
}

// check verifies the challenge. Missing: 400 SCH_REQUIRED_FIELD_MISSING. Wrong
// method: 400 SCH_INVALID_FORMAT. Unreachable: 502. Not "True": 400
// BIZ_GENERIC_ERROR (not 401, which means a bad Beckn signature).
func (v *otpVerify) check(ctx context.Context, exchange common.Exchange) error {
	kind, identity, method, otp := challengeIn(exchange)
	if kind == "" || identity == "" || otp == "" {
		return model.NewBadReqErr("SCH_REQUIRED_FIELD_MISSING", errors.New(
			"this call needs the farmer's identifier and challenge.value, the OTP the farmer received"))
	}
	if method != challengeMethod {
		return model.NewBadReqErr("SCH_INVALID_FORMAT", fmt.Errorf(
			"challenge.method must be %s", challengeMethod))
	}

	base, err := v.host(ctx, exchange)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{"Types": kind, "Values": identity, "OTP": otp})
	if err != nil {
		return err
	}
	sealed, open, err := v.codec.seal(ctx, body)
	if err != nil {
		return err
	}

	hostAction := v.hostAction
	if hostAction == "" {
		hostAction = exchange.Action
	}
	var call model.ActionPlan
	if exchange.Plan != nil {
		call = exchange.Plan.Actions[hostAction]
	}
	answer, err := postSealed(ctx, v.client, v.provider, "OTP verification", base, v.path, call, sealed)
	if err != nil {
		return err
	}
	plain, err := open(ctx, answer)
	if err != nil {
		return err
	}
	if !succeeded(plain) {
		// The portal's message is never logged or returned.
		log.Warnf(ctx, "%s refused the OTP", v.provider)
		return model.NewCodedErr(http.StatusBadRequest, "BIZ_GENERIC_ERROR",
			errors.New("the OTP is wrong or has expired"))
	}
	return nil
}

// challengeIn reads the identifier and challenge from support's channel or the
// commitment's attributes: enrolmentId, else applicant.mobile, else
// applicant.aadhaar. The mapping has already refused more than one; kind is
// empty when there is none.
func challengeIn(exchange common.Exchange) (kind, identity, method, otp string) {
	payload, _ := exchange.Beckn.(map[string]any)
	message, _ := payload["message"].(map[string]any)

	var holder map[string]any
	if support, isSupport := message["support"].(map[string]any); isSupport {
		if channels, ok := support["channels"].([]any); ok && len(channels) > 0 {
			holder, _ = channels[0].(map[string]any)
		}
	} else if contract, isContract := message["contract"].(map[string]any); isContract {
		if commitments, ok := contract["commitments"].([]any); ok && len(commitments) > 0 {
			commitment, _ := commitments[0].(map[string]any)
			holder, _ = commitment["commitmentAttributes"].(map[string]any)
		}
	}
	if id := text(holder["enrolmentId"]); id != "" {
		kind, identity = identityType, id
	} else if applicant, ok := holder["applicant"].(map[string]any); ok {
		for _, slot := range []string{"mobile", "aadhaar"} {
			if id := text(applicant[slot]); id != "" {
				kind, identity = applicantTypes[slot], id
				break
			}
		}
	}
	if challenge, ok := holder["challenge"].(map[string]any); ok {
		method, otp = text(challenge["method"]), text(challenge["value"])
	}
	return kind, identity, method, otp
}

// succeeded is true only for a "True" flag; anything else fails closed.
func succeeded(plain []byte) bool {
	var answer map[string]any
	if json.Unmarshal(plain, &answer) != nil {
		return false
	}
	for _, key := range []string{"Rsponce", "Responce", "Status", "status"} {
		if value, ok := answer[key]; ok {
			return strings.EqualFold(text(value), "true")
		}
	}
	return false
}

// text returns a trimmed string, or "" if the value is not one.
func text(value any) string {
	s, _ := value.(string)
	return strings.TrimSpace(s)
}
