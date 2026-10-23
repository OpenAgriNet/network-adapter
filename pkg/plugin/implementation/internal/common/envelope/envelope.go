// Package envelope encrypts a provider's bodies, verifies a challenge before
// the call and makes follow-up reads, so mappings stay plain JSON (ADR-0001).
package envelope

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
)

// Envelope kinds. See gcm.go and cbc.go.
const (
	envelopeAESGCM          = "aesGcm"          // fixed key and nonce
	envelopeAESCBCKeyInBand = "aesCbcKeyInBand" // fresh key per request, sent as cipher@key
)

// codeInternal is returned when the adapter itself is misconfigured.
const codeInternal = "NET_INTERNAL_ERROR"

// envelopeFields maps each provider-block setting to its field. envelope* is the
// default codec, otp* a separate OTP service, followUp* a second read.
var envelopeFields = map[string]func(*envelopeSettings, string){
	"envelope":              func(s *envelopeSettings, v string) { s.kind = v },
	"envelopeKeyEnv":        func(s *envelopeSettings, v string) { s.keyEnv = v },
	"envelopeNonceEnv":      func(s *envelopeSettings, v string) { s.nonceEnv = v },
	"envelopeTokenEnv":      func(s *envelopeSettings, v string) { s.tokenEnv = v },
	"envelopeTokenField":    func(s *envelopeSettings, v string) { s.tokenField = v },
	"envelopeRequestField":  func(s *envelopeSettings, v string) { s.requestField = v },
	"envelopeResponsePaths": func(s *envelopeSettings, v string) { s.responsePaths = v },

	"otpEnvelope":        func(s *envelopeSettings, v string) { s.otp.kind = v },
	"otpBindingKey":      func(s *envelopeSettings, v string) { s.otp.bindingKey = v },
	"otpTokenEnv":        func(s *envelopeSettings, v string) { s.otp.tokenEnv = v },
	"otpTokenField":      func(s *envelopeSettings, v string) { s.otp.tokenField = v },
	"otpRequestField":    func(s *envelopeSettings, v string) { s.otp.requestField = v },
	"otpResponsePaths":   func(s *envelopeSettings, v string) { s.otp.responsePaths = v },
	"otpActions":         func(s *envelopeSettings, v string) { s.otpActions = v },
	"otpVerifyPath":      func(s *envelopeSettings, v string) { s.verifyPath = v },
	"otpVerifyActions":   func(s *envelopeSettings, v string) { s.verifyActions = v },
	"otpVerifyTimeoutMs": func(s *envelopeSettings, v string) { s.verifyTimeoutMs = v },

	"followUpPath":      func(s *envelopeSettings, v string) { s.followUpPath = v },
	"followUpField":     func(s *envelopeSettings, v string) { s.followUpField = v },
	"followUpActions":   func(s *envelopeSettings, v string) { s.followUpActions = v },
	"followUpTimeoutMs": func(s *envelopeSettings, v string) { s.followUpTimeoutMs = v },
}

// envelopeSettings is one provider's envelope as written in config.
type envelopeSettings struct {
	kind, keyEnv, nonceEnv, tokenEnv, tokenField, requestField, responsePaths string

	otp             realmSettings
	otpActions      string
	verifyPath      string
	verifyActions   string
	verifyTimeoutMs string

	followUpPath, followUpField, followUpActions, followUpTimeoutMs string
}

// realmSettings configures a CBC codec. bindingKey names the registry binding
// whose participant is the OTP service, a host other than the provider's.
type realmSettings struct {
	kind, bindingKey, tokenEnv, tokenField, requestField, responsePaths string
}

// Parsed is each provider's envelope and the actions it sends elsewhere, for
// common.Config's EnvelopeByProvider and HostedElsewhere.
type Parsed struct {
	Envelopes       map[string]common.Envelope
	HostedElsewhere map[string]map[string]string
}

// Parse reads each provider's "setting-provider" envelope settings and returns
// the rest.
func Parse(config map[string]string, hooks Hooks) (Parsed, map[string]string, error) {
	rest := make(map[string]string, len(config))
	settings := map[string]*envelopeSettings{}
	for key, value := range config {
		field, provider, dashed := strings.Cut(key, "-")
		set, isEnvelope := envelopeFields[field]
		if !dashed || !isEnvelope {
			rest[key] = value
			continue
		}
		if settings[provider] == nil {
			settings[provider] = &envelopeSettings{}
		}
		set(settings[provider], value)
	}
	if len(settings) == 0 {
		return Parsed{}, rest, nil
	}

	parsed := Parsed{Envelopes: make(map[string]common.Envelope, len(settings))}
	for provider, s := range settings {
		e, err := newProviderEnvelope(provider, *s, hooks)
		if err != nil {
			return Parsed{}, nil, err
		}
		parsed.Envelopes[provider] = e
		if hosted := e.hostedElsewhere(); len(hosted) > 0 {
			if parsed.HostedElsewhere == nil {
				parsed.HostedElsewhere = map[string]map[string]string{}
			}
			parsed.HostedElsewhere[provider] = hosted
		}
	}
	return parsed, rest, nil
}

// codec seals a request and returns the opener for its answer.
type codec interface {
	seal(ctx context.Context, mapped []byte) ([]byte, common.Opener, error)
}

// providerEnvelope picks a codec per action and runs the OTP verify and
// follow-up read where configured. A nil field means that feature is off.
type providerEnvelope struct {
	provider      string
	base          codec // nil: plain JSON
	otp           codec
	otpBindingKey string
	otpActions    map[string]bool
	verify        *verify
	followUp      *followUp
}

var _ common.Envelope = (*providerEnvelope)(nil)

// hostedElsewhere sends the OTP actions to the OTP service's participant.
func (e *providerEnvelope) hostedElsewhere() map[string]string {
	if e.otpBindingKey == "" {
		return nil
	}
	hosted := make(map[string]string, len(e.otpActions))
	for action := range e.otpActions {
		hosted[action] = e.otpBindingKey
	}
	return hosted
}

// otpHost is the OTP service's base URL, read from the registry per call.
func (e *providerEnvelope) otpHost(ctx context.Context, exchange common.Exchange) (string, error) {
	return common.HostOf(ctx, exchange.Registry, e.otpBindingKey)
}

// hostFor is where an action's calls go: the OTP service for an OTP action,
// the participant for the rest.
func (e *providerEnvelope) hostFor(ctx context.Context, exchange common.Exchange) (string, error) {
	if e.otpActions[exchange.Action] {
		return e.otpHost(ctx, exchange)
	}
	return participantHost(ctx, exchange)
}

// Seal verifies the OTP if the action needs it, then seals the request. A wrong
// OTP stops the call.
func (e *providerEnvelope) Seal(ctx context.Context, exchange common.Exchange, mapped []byte) ([]byte, common.Opener, error) {
	if e.verify != nil && e.verify.actions[exchange.Action] {
		if err := e.verify.check(ctx, exchange); err != nil {
			return nil, nil, err
		}
	}
	c := e.base
	if e.otpActions[exchange.Action] {
		c = e.otp
	}
	if c == nil {
		return mapped, func(_ context.Context, answer []byte) ([]byte, error) { return answer, nil }, nil
	}
	sealed, open, err := c.seal(ctx, mapped)
	if err != nil || e.followUp == nil || !e.followUp.actions[exchange.Action] {
		return sealed, open, err
	}
	base, err := e.hostFor(ctx, exchange)
	if err != nil {
		return nil, nil, err
	}
	return sealed, e.followUp.wrap(c, base, mapped, open), nil
}

// newProviderEnvelope validates the settings. Secrets are read at call time.
func newProviderEnvelope(provider string, s envelopeSettings, hooks Hooks) (*providerEnvelope, error) {
	if hooks.Succeeded == nil {
		return nil, fmt.Errorf("%s: the envelope needs Hooks.Succeeded", provider)
	}
	e := &providerEnvelope{provider: provider, otpBindingKey: strings.TrimSpace(s.otp.bindingKey),
		otpActions: listSet(s.otpActions)}
	// The verify and follow-up calls carry the farmer's identifier, so they
	// refuse an off-host redirect as the main call does.
	client := &http.Client{CheckRedirect: util.RefuseOffHostRedirect}

	switch s.kind {
	case "":
		// Without the kind the body would go out unsealed: refuse at startup.
		if s.keyEnv != "" || s.nonceEnv != "" || s.tokenEnv != "" || s.tokenField != "" ||
			s.requestField != "" || s.responsePaths != "" {
			return nil, fmt.Errorf("%s: envelope settings are present but envelope is not set", provider)
		}
	case envelopeAESCBCKeyInBand:

		if s.keyEnv != "" || s.nonceEnv != "" {
			return nil, fmt.Errorf("%s: envelope %s carries its key in band; drop envelopeKeyEnv and envelopeNonceEnv",
				provider, envelopeAESCBCKeyInBand)
		}
		base, err := newCBCCodecFrom(provider, "envelope", "envelope", realmSettings{kind: s.kind,
			tokenEnv: s.tokenEnv, tokenField: s.tokenField, requestField: s.requestField, responsePaths: s.responsePaths})
		if err != nil {
			return nil, err
		}
		e.base = base
	default:
		base, err := newGCMCodec(provider, s)
		if err != nil {
			return nil, err
		}
		e.base = base
	}

	if s.otp.kind != "" {
		otp, err := newCBCCodec(provider, s.otp)
		if err != nil {
			return nil, err
		}
		if len(e.otpActions) == 0 {
			return nil, fmt.Errorf("%s: otpEnvelope needs otpActions, the actions sent through it", provider)
		}
		participant, capability, found := strings.Cut(e.otpBindingKey, "|")
		if !found || participant == "" || capability == "" {
			return nil, fmt.Errorf("%s: otpEnvelope needs otpBindingKey, the binding key (participant|capability) "+
				"whose participant is the OTP service; got %q", provider, e.otpBindingKey)
		}
		e.otp = otp
	} else if len(e.otpActions) > 0 || e.otpBindingKey != "" || s.otp.tokenEnv != "" || s.otp.requestField != "" || s.otp.responsePaths != "" {
		return nil, fmt.Errorf("%s: otp settings are present but otpEnvelope is not set", provider)
	}

	verifyTimeoutMs, err := timeoutSetting(provider, "otpVerifyTimeoutMs", s.verifyTimeoutMs)
	if err != nil {
		return nil, err
	}
	followUpTimeoutMs, err := timeoutSetting(provider, "followUpTimeoutMs", s.followUpTimeoutMs)
	if err != nil {
		return nil, err
	}

	verifyActions := listSet(s.verifyActions)
	switch {
	case s.verifyPath == "" && len(verifyActions) == 0:
	case s.verifyPath == "" || len(verifyActions) == 0:
		return nil, fmt.Errorf("%s: otpVerifyPath and otpVerifyActions are set together or not at all", provider)
	case hooks.Challenge == nil:
		return nil, fmt.Errorf("%s: otpVerifyPath needs Hooks.Challenge, to read the OTP from a payload", provider)
	case e.otp != nil:
		// Separate OTP service: verify there.
		e.verify = &verify{provider: provider, path: s.verifyPath, actions: verifyActions, codec: e.otp.(*cbcCodec),
			host: e.otpHost, hooks: hooks, client: client, budget: model.ActionPlan{TimeoutMs: verifyTimeoutMs}}
	default:
		// The participant is the OTP service: verify on its host.
		cbc, onOTPService := e.base.(*cbcCodec)
		if !onOTPService {
			return nil, fmt.Errorf("%s: otpVerifyPath needs otpEnvelope, or envelope %s: the OTP is verified on the OTP service",
				provider, envelopeAESCBCKeyInBand)
		}
		e.verify = &verify{provider: provider, path: s.verifyPath, actions: verifyActions, codec: cbc,
			host: participantHost, hooks: hooks, client: client, budget: model.ActionPlan{TimeoutMs: verifyTimeoutMs}}
	}

	followUpActions := listSet(s.followUpActions)
	switch {
	case s.followUpPath == "" && s.followUpField == "" && len(followUpActions) == 0:
	case s.followUpPath == "" || s.followUpField == "" || len(followUpActions) == 0:
		return nil, fmt.Errorf("%s: followUpPath, followUpField and followUpActions are set together or not at all", provider)
	default:
		for action := range followUpActions {
			if e.otpActions[action] && e.otp == nil || !e.otpActions[action] && e.base == nil {
				return nil, fmt.Errorf("%s: followUpActions names %s, which has no envelope to send a follow-up through", provider, action)
			}
		}
		e.followUp = &followUp{provider: provider, path: s.followUpPath, field: s.followUpField, actions: followUpActions,
			succeeded: hooks.Succeeded, client: client, budget: model.ActionPlan{TimeoutMs: followUpTimeoutMs}}
	}

	if e.base == nil && e.otp == nil {
		return nil, fmt.Errorf("%s: an envelope block needs envelope or otpEnvelope", provider)
	}
	return e, nil
}

// participantHost is the registry's base URL for the provider.
func participantHost(_ context.Context, exchange common.Exchange) (string, error) {
	if exchange.Plan == nil || exchange.Plan.BaseURL == "" {
		return "", errors.New("no call plan to find the provider's host on")
	}
	return exchange.Plan.BaseURL, nil
}

// --- shared by the codecs ----------------------------------------------------

// withToken adds the service token to the plaintext. It is added here, not in
// the mapping, because mapping files are public.
func withToken(ctx context.Context, provider string, mapped []byte, tokenEnv, tokenField string) ([]byte, error) {
	fields := map[string]any{}
	if len(mapped) > 0 {
		if err := json.Unmarshal(mapped, &fields); err != nil {
			return nil, fmt.Errorf("the %s request mapping must produce a JSON object: %w", provider, err)
		}
	}
	if tokenField != "" {
		token := os.Getenv(tokenEnv)
		if token == "" {
			return nil, unconfigured(ctx, provider, tokenEnv+" is not set")
		}
		if _, present := fields[tokenField]; present {
			// A public mapping must never carry the token.
			return nil, fmt.Errorf("the %s request mapping sets %s, which the envelope supplies; "+
				"remove it from the mapping", provider, tokenField)
		}
		fields[tokenField] = token
	}
	plain, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("the %s request could not be encoded: %w", provider, err)
	}
	return plain, nil
}

// checkToken requires tokenEnv and tokenField together or not at all.
func checkToken(tokenEnv, tokenField string, missing []string, prefix string) []string {
	if (tokenEnv == "") != (tokenField == "") {
		return append(missing, "both "+prefix+"TokenEnv and "+prefix+"TokenField, or neither")
	}
	return missing
}

// parsePaths reads a comma-separated list of dotted paths.
func parsePaths(provider, setting, raw string) ([][]string, error) {
	var paths [][]string
	for _, path := range strings.Split(raw, ",") {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		segments := strings.Split(path, ".")
		for _, segment := range segments {
			if strings.TrimSpace(segment) == "" {
				return nil, fmt.Errorf("%s: %s %q has a blank segment", provider, setting, path)
			}
		}
		paths = append(paths, segments)
	}
	return paths, nil
}

// sealedAnswer finds the sealed string at the first path that holds one.
func sealedAnswer(wire any, paths [][]string) (string, bool) {
	for _, path := range paths {
		node := wire
		for _, key := range path {
			fields, ok := node.(map[string]any)
			if !ok {
				node = nil
				break
			}
			node = fields[key]
		}
		if value, ok := node.(string); ok {
			return value, true
		}
	}
	return "", false
}

// unconfigured returns a 500; the detail goes to the log, not the wire.
func unconfigured(ctx context.Context, provider, detail string) error {
	err := fmt.Errorf("the envelope for %s is not configured on this adapter", provider)
	log.Errorf(ctx, err, "%s", detail)
	return model.NewCodedErr(http.StatusInternalServerError, codeInternal, err)
}

// redactToken removes a service token from text about to be logged.
func redactToken(text, tokenEnv string) string {
	if tokenEnv == "" {
		return text
	}
	if token := os.Getenv(tokenEnv); token != "" {
		text = strings.ReplaceAll(text, token, util.RedactedMarker)
	}
	return text
}

// refused is a 502: the provider's answer is unusable.
func refused(err error) error {
	return model.NewCodedErr(http.StatusBadGateway, util.CodeUpstreamUnavailable, err)
}

// decodeHex decodes hex, ignoring whitespace.
func decodeHex(raw string) ([]byte, error) {
	cleaned := strings.Join(strings.Fields(raw), "")
	if cleaned == "" {
		return nil, errors.New("empty")
	}
	return hex.DecodeString(cleaned)
}

// listSet reads a comma-separated list into a set.
func listSet(raw string) map[string]bool {
	set := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			set[trimmed] = true
		}
	}
	return set
}

// missingSettings lists empty settings, sorted.
func missingSettings(settings map[string]string) []string {
	var missing []string
	for setting, value := range settings {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, setting)
		}
	}
	sort.Strings(missing)
	return missing
}

// timeoutSetting reads a call's own budget in milliseconds. Unset is 0, which
// util.Budget takes as its default.
func timeoutSetting(provider, name, value string) (int, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil
	}
	ms, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || ms <= 0 {
		return 0, fmt.Errorf("%s: %s must be a positive number of milliseconds, got %q", provider, name, value)
	}
	return ms, nil
}
