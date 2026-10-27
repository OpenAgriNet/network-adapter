package Grievance

import (
	"encoding/json"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/envelope"
)

// challengeMethod is the only accepted challenge.
const challengeMethod = "OTP"

// identityType is the OTP service's type for a registration number (enrolmentId).
// The PM-KISAN mappings write the same value as a literal; change them together.
const identityType = "Ben_id"

// mobileType is the OTP service's type for applicant.mobile. Also a literal in
// the PM-KISAN mappings; change them together.
const mobileType = "Mobile"

// pmkisanHooks are the grievance packs' side of the envelope: where the challenge
// sits in a payload, and what PM-KISAN's "yes" looks like.
var pmkisanHooks = envelope.Hooks{Challenge: verifyRequest, Succeeded: pmkisanSaidYes}

// ParseEnvelopes reads each provider's envelope and follow-up settings out of
// the plugin config. See envelope.Parse.
func ParseEnvelopes(config map[string]string) (envelope.Parsed, map[string]string, error) {
	followUps, config := splitFollowUps(config)
	parsed, rest, err := envelope.Parse(config, pmkisanHooks)
	if err != nil {
		return envelope.Parsed{}, nil, err
	}
	for provider, s := range followUps {
		wrapped, err := newFollowUp(provider, *s, parsed.Envelopes[provider])
		if err != nil {
			return envelope.Parsed{}, nil, err
		}
		parsed.Envelopes[provider] = wrapped
	}
	return parsed, rest, nil
}

// verifyRequest is the OTP service's verify body: identifier, its type and
// the OTP. Missing or a wrong method is a 400 naming the field. This is the
// only check that the OTP is present; the mappings do not repeat it.
func verifyRequest(exchange common.Exchange) (map[string]any, error) {
	at, kind, identity, method, otp := challengeIn(exchange)
	switch {
	case kind == "" || identity == "":
		return nil, refuseField("SCH_REQUIRED_FIELD_MISSING", "this call needs the farmer's identifier", at+".enrolmentId")
	case otp == "":
		return nil, refuseField("SCH_REQUIRED_FIELD_MISSING",
			"challenge.response.value is required: the OTP the farmer received", at+".challenge.response.value")
	case method != challengeMethod:
		return nil, refuseField("SCH_INVALID_FORMAT", "challenge.response.method must be "+challengeMethod,
			at+".challenge.response.method")
	}
	return map[string]any{"Types": kind, "Values": identity, "OTP": otp}, nil
}

// refuseField is a 400 whose details.path names the field, as a mapping's
// _error with a path is.
func refuseField(code, message, path string) error {
	return &model.SchemaValidationErr{Errors: []model.Error{*model.NewCodedErrorWithCause(code, message, path, nil)}}
}

// challengeIn reads enrolmentId (else applicant.mobile) and the challenge from
// support's channel or the commitment's attributes; at is the JSONPath of the
// one it read.
func challengeIn(exchange common.Exchange) (at, kind, identity, method, otp string) {
	payload, _ := exchange.Beckn.(map[string]any)
	message, _ := payload["message"].(map[string]any)

	var holder map[string]any
	if support, isSupport := message["support"].(map[string]any); isSupport {
		if channels, ok := support["channels"].([]any); ok && len(channels) > 0 {
			holder, _ = channels[0].(map[string]any)
		}
		at = "$.message.support.channels[0]"
	} else if contract, isContract := message["contract"].(map[string]any); isContract {
		if commitments, ok := contract["commitments"].([]any); ok && len(commitments) > 0 {
			commitment, _ := commitments[0].(map[string]any)
			holder, _ = commitment["commitmentAttributes"].(map[string]any)
		}
		at = "$.message.contract.commitments[0].commitmentAttributes"
	}
	if id := trimmedString(holder["enrolmentId"]); id != "" {
		kind, identity = identityType, id
	} else if applicant, ok := holder["applicant"].(map[string]any); ok {
		if id := trimmedString(applicant["mobile"]); id != "" {
			kind, identity = mobileType, id
		}
	}
	challenge, _ := holder["challenge"].(map[string]any)
	if response, ok := challenge["response"].(map[string]any); ok {
		method, otp = trimmedString(response["method"]), trimmedString(response["value"])
	}
	return at, kind, identity, method, otp
}

// pmkisanSaidYes is true only for a "True" flag; anything else fails closed. The
// PM-KISAN mappings apply the same rule as $statusText.
func pmkisanSaidYes(plain []byte) bool {
	var answer map[string]any
	if json.Unmarshal(plain, &answer) != nil {
		return false
	}
	for _, key := range []string{"Rsponce", "Responce", "Status", "status"} {
		if value, ok := answer[key]; ok {
			return strings.EqualFold(trimmedString(value), "true")
		}
	}
	return false
}

// trimmedString returns a trimmed string, or "" if the value is not one.
func trimmedString(value any) string {
	s, _ := value.(string)
	return strings.TrimSpace(s)
}
