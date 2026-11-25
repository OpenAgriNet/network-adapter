package Grievance

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/envelope"
)

// challengeFields are a provider block's OTP-verify settings: the names the
// provider's own verify body uses and where in a Beckn payload each value is
// read from. They are config and not constants because the verify body is the
// provider's, not this capability's -- holding them here would put one
// provider's field names in code every provider shares.
var challengeFields = map[string]func(*challengeSettings, string){
	"otpVerifyBodyFields":   func(s *challengeSettings, v string) { s.bodyFields = v },
	"otpVerifyIdentityFrom": func(s *challengeSettings, v string) { s.identityFrom = v },
	"otpVerifyOtpFrom":      func(s *challengeSettings, v string) { s.otpFrom = v },
	"otpVerifyMethodFrom":   func(s *challengeSettings, v string) { s.methodFrom = v },
}

type challengeSettings struct{ bodyFields, identityFrom, otpFrom, methodFrom string }

// identitySource is one place an identifier may sit and what the provider
// calls an identifier found there.
type identitySource struct{ at, kind string }

// challengeSpec is one provider's verify body, resolved from its settings.
type challengeSpec struct {
	// kindField, valueField and otpField are the provider's own names for the
	// kind of identifier, the identifier and the OTP.
	kindField, valueField, otpField string
	// identities are tried in order; the first that holds a value wins.
	identities []identitySource
	otpAt      string
	// methodAt and method are optional: when set, the payload must carry
	// exactly that method, which is how a caller is told it sent the wrong
	// kind of challenge rather than a wrong OTP.
	methodAt, method string
}

// hooks are this capability's side of the envelope: how a challenge is read out
// of a payload, per provider, and what a provider's "yes" looks like.
func hooks(specs map[string]*challengeSpec) envelope.Hooks {
	return envelope.Hooks{
		Challenge: func(provider string, exchange common.Exchange) (map[string]any, error) {
			spec := specs[provider]
			if spec == nil {
				return nil, fmt.Errorf("%s: otpVerifyPath is set but its otpVerify* field settings are not", provider)
			}
			return spec.request(exchange)
		},
		Succeeded: pmkisanSaidYes,
	}
}

// ParseEnvelopes reads each provider's envelope, challenge and follow-up
// settings out of the plugin config. See envelope.Parse.
func ParseEnvelopes(config map[string]string) (envelope.Parsed, map[string]string, error) {
	challenges, config := splitChallenges(config)
	followUps, config := splitFollowUps(config)

	specs := make(map[string]*challengeSpec, len(challenges))
	for provider, s := range challenges {
		spec, err := newChallengeSpec(provider, *s)
		if err != nil {
			return envelope.Parsed{}, nil, err
		}
		specs[provider] = spec
	}
	// A verify with no field settings would only fail on the first farmer to
	// present an OTP, so it fails here instead.
	for key := range config {
		if setting, provider, dashed := strings.Cut(key, "-"); dashed && setting == "otpVerifyPath" && specs[provider] == nil {
			return envelope.Parsed{}, nil, fmt.Errorf(
				"%s: otpVerifyPath needs otpVerifyBodyFields, otpVerifyIdentityFrom and otpVerifyOtpFrom", provider)
		}
	}

	parsed, rest, err := envelope.Parse(config, hooks(specs))
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

// splitChallenges takes the OTP-verify field settings out of the plugin config.
func splitChallenges(config map[string]string) (map[string]*challengeSettings, map[string]string) {
	settings := map[string]*challengeSettings{}
	rest := make(map[string]string, len(config))
	for key, value := range config {
		field, provider, dashed := strings.Cut(key, "-")
		set, isChallenge := challengeFields[field]
		if !dashed || !isChallenge {
			rest[key] = value
			continue
		}
		if settings[provider] == nil {
			settings[provider] = &challengeSettings{}
		}
		set(settings[provider], value)
	}
	return settings, rest
}

// newChallengeSpec validates one provider's settings.
//
//	otpVerifyBodyFields:   "Types, Values, OTP"
//	otpVerifyIdentityFrom: "enrolmentId=Ben_id, applicant.mobile=Mobile"
//	otpVerifyOtpFrom:      "challenge.response.value"
//	otpVerifyMethodFrom:   "challenge.response.method=OTP"   (optional)
func newChallengeSpec(provider string, s challengeSettings) (*challengeSpec, error) {
	body := splitList(s.bodyFields)
	if len(body) != 3 {
		return nil, fmt.Errorf("%s: otpVerifyBodyFields needs three names -- the kind, the identifier and the OTP "+
			"as the provider's verify body spells them; got %q", provider, s.bodyFields)
	}
	spec := &challengeSpec{kindField: body[0], valueField: body[1], otpField: body[2],
		otpAt: strings.TrimSpace(s.otpFrom)}
	if spec.otpAt == "" {
		return nil, fmt.Errorf("%s: otpVerifyOtpFrom needs the payload path the OTP is read from", provider)
	}
	for _, pair := range splitList(s.identityFrom) {
		at, kind, paired := strings.Cut(pair, "=")
		at, kind = strings.TrimSpace(at), strings.TrimSpace(kind)
		if !paired || at == "" || kind == "" {
			return nil, fmt.Errorf("%s: otpVerifyIdentityFrom takes path=kind pairs, the provider's name for an "+
				"identifier found at each path; got %q", provider, pair)
		}
		spec.identities = append(spec.identities, identitySource{at: at, kind: kind})
	}
	if len(spec.identities) == 0 {
		return nil, fmt.Errorf("%s: otpVerifyIdentityFrom needs at least one path=kind pair", provider)
	}
	if raw := strings.TrimSpace(s.methodFrom); raw != "" {
		at, method, paired := strings.Cut(raw, "=")
		spec.methodAt, spec.method = strings.TrimSpace(at), strings.TrimSpace(method)
		if !paired || spec.methodAt == "" || spec.method == "" {
			return nil, fmt.Errorf("%s: otpVerifyMethodFrom takes one path=method pair, the only challenge method "+
				"accepted; got %q", provider, raw)
		}
	}
	return spec, nil
}

// request is the provider's verify body: the identifier, its kind and the OTP,
// each under the name the provider uses. Missing or a wrong method is a 400
// naming the field. This is the only check that the OTP is present; the
// mappings do not repeat it.
func (spec *challengeSpec) request(exchange common.Exchange) (map[string]any, error) {
	at, holder := challengeHolder(exchange)
	kind, identity := "", ""
	for _, source := range spec.identities {
		if value := trimmedString(valueAt(holder, source.at)); value != "" {
			kind, identity = source.kind, value
			break
		}
	}
	otp := trimmedString(valueAt(holder, spec.otpAt))

	switch {
	case identity == "":
		return nil, refuseField("SCH_REQUIRED_FIELD_MISSING", "this call needs the farmer's identifier",
			at+"."+spec.identities[0].at)
	case otp == "":
		return nil, refuseField("SCH_REQUIRED_FIELD_MISSING",
			spec.otpAt+" is required: the OTP the farmer received", at+"."+spec.otpAt)
	}
	if spec.methodAt != "" {
		if method := trimmedString(valueAt(holder, spec.methodAt)); method != spec.method {
			return nil, refuseField("SCH_INVALID_FORMAT", spec.methodAt+" must be "+spec.method, at+"."+spec.methodAt)
		}
	}
	return map[string]any{spec.kindField: kind, spec.valueField: identity, spec.otpField: otp}, nil
}

// refuseField is a 400 whose details.path names the field, as a mapping's
// _error with a path is.
func refuseField(code, message, path string) error {
	return &model.SchemaValidationErr{Errors: []model.Error{*model.NewCodedErrorWithCause(code, message, path, nil)}}
}

// challengeHolder is where a challenge sits in a Beckn payload -- support's
// first channel or the commitment's attributes -- and the JSONPath of the one
// it found. This is Beckn's shape, not a provider's, so it stays in code.
func challengeHolder(exchange common.Exchange) (at string, holder map[string]any) {
	payload, _ := exchange.Beckn.(map[string]any)
	message, _ := payload["message"].(map[string]any)

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
	return at, holder
}

// valueAt walks a dotted path from the holder.
func valueAt(holder map[string]any, path string) any {
	var node any = holder
	for _, key := range strings.Split(path, ".") {
		fields, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		node = fields[key]
	}
	return node
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

// splitList reads a comma-separated list, dropping blanks.
func splitList(raw string) []string {
	var items []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			items = append(items, trimmed)
		}
	}
	return items
}

// trimmedString returns a trimmed string, or "" if the value is not one.
func trimmedString(value any) string {
	s, _ := value.(string)
	return strings.TrimSpace(s)
}
