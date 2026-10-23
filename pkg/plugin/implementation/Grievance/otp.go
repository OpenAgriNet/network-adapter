package Grievance

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/envelope"
)

// challengeMethod is the only accepted challenge.
const challengeMethod = "SMS_OTP"

// identityType is the OTP service's type for a registration number (enrolmentId).
const identityType = "Ben_id"

// mobileType is the OTP service's type for applicant.mobile.
const mobileType = "Mobile"

// hooks are the grievance packs' side of the envelope: where the challenge
// sits in a payload, and what PM-KISAN's "yes" looks like.
var hooks = envelope.Hooks{Challenge: verifyRequest, Succeeded: succeeded}

// ParseEnvelopes reads each provider's envelope settings out of the plugin
// config. See envelope.Parse.
func ParseEnvelopes(config map[string]string) (envelope.Parsed, map[string]string, error) {
	return envelope.Parse(config, hooks)
}

// verifyRequest is the OTP service's verify body: identifier, its type and
// the OTP. Missing or a wrong method is a 400.
func verifyRequest(exchange common.Exchange) (map[string]any, error) {
	kind, identity, method, otp := challengeIn(exchange)
	if kind == "" || identity == "" || otp == "" {
		return nil, model.NewBadReqErr("SCH_REQUIRED_FIELD_MISSING", errors.New(
			"this call needs the farmer's identifier and challenge.response.value, the OTP the farmer received"))
	}
	if method != challengeMethod {
		return nil, model.NewBadReqErr("SCH_INVALID_FORMAT", fmt.Errorf(
			"challenge.response.method must be %s", challengeMethod))
	}
	return map[string]any{"Types": kind, "Values": identity, "OTP": otp}, nil
}

// challengeIn reads enrolmentId (else applicant.mobile) and the challenge from
// support's channel or the commitment's attributes.
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
		if id := text(applicant["mobile"]); id != "" {
			kind, identity = mobileType, id
		}
	}
	challenge, _ := holder["challenge"].(map[string]any)
	if response, ok := challenge["response"].(map[string]any); ok {
		method, otp = text(response["method"]), text(response["value"])
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
