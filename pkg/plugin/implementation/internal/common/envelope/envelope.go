// Package envelope encrypts a provider's bodies, verifies a challenge before
// the call and makes follow-up reads, so mappings stay plain JSON (ADR-0001).
package envelope

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
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
// default codec, otp* a separate OTP service.
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
}

// envelopeSettings is one provider's envelope as written in config.
type envelopeSettings struct {
	kind, keyEnv, nonceEnv, tokenEnv, tokenField, requestField, responsePaths string

	otp             cbcSettings
	otpActions      string
	verifyPath      string
	verifyActions   string
	verifyTimeoutMs string
}

// cbcSettings configures a CBC codec. bindingKey names the registry binding
// whose participant is the OTP service, a host other than the provider's.
type cbcSettings struct {
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

// providerEnvelope picks a codec per action and runs the OTP verify where
// configured. A nil field means that feature is off.
type providerEnvelope struct {
	provider      string
	base          codec // nil: plain JSON
	otp           codec
	otpBindingKey string
	otpActions    map[string]bool
	verify        *verify
}

var (
	_ common.Envelope  = (*providerEnvelope)(nil)
	_ UnverifiedSealer = (*providerEnvelope)(nil)
)

// UnverifiedSealer seals a request for an action WITHOUT running its OTP
// verify: for a second call after a verified first (an OTP is spent once).
// Calling it by mistake skips the OTP check.
type UnverifiedSealer interface {
	SealWithoutVerify(ctx context.Context, exchange common.Exchange, mapped []byte) ([]byte, common.Opener, error)
}

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

// Seal verifies the OTP if the action needs it, then seals the request. A wrong
// OTP stops the call.
func (e *providerEnvelope) Seal(ctx context.Context, exchange common.Exchange, mapped []byte) ([]byte, common.Opener, error) {
	if e.verify != nil && e.verify.actions[exchange.Action] {
		if err := e.verify.check(ctx, exchange); err != nil {
			return nil, nil, err
		}
	}
	return e.SealWithoutVerify(ctx, exchange, mapped)
}

// SealWithoutVerify seals with the action's codec and skips the OTP verify.
func (e *providerEnvelope) SealWithoutVerify(ctx context.Context, exchange common.Exchange, mapped []byte) ([]byte, common.Opener, error) {
	c := e.base
	if e.otpActions[exchange.Action] {
		c = e.otp
	}
	if c == nil {
		return mapped, func(_ context.Context, answer []byte) ([]byte, error) { return answer, nil }, nil
	}
	return c.seal(ctx, mapped)
}

// newProviderEnvelope validates the settings. Secrets are read at call time.
func newProviderEnvelope(provider string, s envelopeSettings, hooks Hooks) (*providerEnvelope, error) {
	if hooks.Succeeded == nil {
		return nil, fmt.Errorf("%s: the envelope needs Hooks.Succeeded", provider)
	}
	e := &providerEnvelope{provider: provider, otpBindingKey: strings.TrimSpace(s.otp.bindingKey),
		otpActions: listSet(s.otpActions)}

	var err error
	if e.base, err = parseBaseCodec(provider, s); err != nil {
		return nil, err
	}
	otp, err := parseOTPCodec(provider, s, e.otpBindingKey, e.otpActions)
	if err != nil {
		return nil, err
	}
	if otp != nil {
		e.otp = otp
	}
	if e.verify, err = parseVerify(provider, s, hooks, otp, e.base, e.otpBindingKey); err != nil {
		return nil, err
	}

	if e.base == nil && e.otp == nil {
		return nil, fmt.Errorf("%s: an envelope block needs envelope or otpEnvelope", provider)
	}
	return e, nil
}

// parseBaseCodec reads the envelope* block: the codec for the provider's own
// calls, or nil for plain JSON.
func parseBaseCodec(provider string, s envelopeSettings) (codec, error) {
	switch s.kind {
	case "":
		// Without the kind the body would go out unsealed.
		if s.keyEnv != "" || s.nonceEnv != "" || s.tokenEnv != "" || s.tokenField != "" ||
			s.requestField != "" || s.responsePaths != "" {
			return nil, fmt.Errorf("%s: envelope settings are present but envelope is not set", provider)
		}
		return nil, nil
	case envelopeAESCBCKeyInBand:
		if s.keyEnv != "" || s.nonceEnv != "" {
			return nil, fmt.Errorf("%s: envelope %s carries its key in band; drop envelopeKeyEnv and envelopeNonceEnv",
				provider, envelopeAESCBCKeyInBand)
		}
		return newCBCCodec(provider, "envelope", cbcSettings{kind: s.kind, tokenEnv: s.tokenEnv,
			tokenField: s.tokenField, requestField: s.requestField, responsePaths: s.responsePaths})
	default:
		return newGCMCodec(provider, s)
	}
}

// parseOTPCodec reads the otp* block: the codec for the OTP service, or nil.
func parseOTPCodec(provider string, s envelopeSettings, bindingKey string, actions map[string]bool) (*cbcCodec, error) {
	if s.otp.kind == "" {
		if len(actions) > 0 || bindingKey != "" || s.otp.tokenEnv != "" || s.otp.requestField != "" || s.otp.responsePaths != "" {
			return nil, fmt.Errorf("%s: otp settings are present but otpEnvelope is not set", provider)
		}
		return nil, nil
	}
	otp, err := newCBCCodec(provider, "otp", s.otp)
	if err != nil {
		return nil, err
	}
	if len(actions) == 0 {
		return nil, fmt.Errorf("%s: otpEnvelope needs otpActions, the actions sent through it", provider)
	}
	participant, capability, found := strings.Cut(bindingKey, "|")
	if !found || participant == "" || capability == "" {
		return nil, fmt.Errorf("%s: otpEnvelope needs otpBindingKey, the binding key (participant|capability) "+
			"whose participant is the OTP service; got %q", provider, bindingKey)
	}
	return otp, nil
}

// parseVerify reads the otpVerify* block: the OTP check before guarded actions,
// or nil. It runs on the OTP service (otp), or on the provider itself when the
// provider's own envelope is CBC.
func parseVerify(provider string, s envelopeSettings, hooks Hooks, otp *cbcCodec, base codec, bindingKey string) (*verify, error) {
	timeoutMs, err := timeoutSetting(provider, "otpVerifyTimeoutMs", s.verifyTimeoutMs)
	if err != nil {
		return nil, err
	}
	actions := listSet(s.verifyActions)
	switch {
	case s.verifyPath == "" && len(actions) == 0:
		return nil, nil
	case s.verifyPath == "" || len(actions) == 0:
		return nil, fmt.Errorf("%s: otpVerifyPath and otpVerifyActions are set together or not at all", provider)
	case hooks.Challenge == nil:
		return nil, fmt.Errorf("%s: otpVerifyPath needs Hooks.Challenge, to read the OTP from a payload", provider)
	}
	v := &verify{provider: provider, path: s.verifyPath, actions: actions, codec: otp, bindingKey: bindingKey,
		hooks: hooks, client: NewClient(), budget: model.ActionPlan{TimeoutMs: timeoutMs}}
	if otp == nil {
		baseCBC, keyInBand := base.(*cbcCodec)
		if !keyInBand {
			return nil, fmt.Errorf("%s: otpVerifyPath needs otpEnvelope, or envelope %s: the OTP is verified on the OTP service",
				provider, envelopeAESCBCKeyInBand)
		}
		v.codec, v.bindingKey = baseCBC, "" // the provider is the OTP service: its own host
	}
	return v, nil
}

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

// sealedIn finds the sealed string at the first of paths that holds one.
// Not JSON, or no envelope: 502.
func sealedIn(ctx context.Context, provider string, answer []byte, paths [][]string) (string, error) {
	var wire any
	if err := json.Unmarshal(answer, &wire); err != nil {
		log.Warnf(ctx, "%s answered with a body that is not JSON (%d bytes)", provider, len(answer))
		return "", refused(fmt.Errorf("%s answered without an envelope", provider))
	}
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
			return strings.TrimSpace(value), nil
		}
	}
	log.Warnf(ctx, "%s answered with no envelope at any configured path", provider)
	return "", refused(fmt.Errorf("%s answered without an envelope", provider))
}

// decodeSealed base64-decodes a sealed string. Plain text there is the
// provider's rejection: logged redacted, never returned (502).
func decodeSealed(ctx context.Context, provider, tokenEnv, sealed string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil || len(raw) == 0 {
		log.Warnf(ctx, "%s answered with plain text where its envelope belongs: %s",
			provider, redactToken(util.Explain([]byte(sealed)), tokenEnv))
		return nil, refused(fmt.Errorf("%s rejected the request", provider))
	}
	return raw, nil
}

// plainJSON checks that a decrypted answer is JSON (502 if not).
func plainJSON(ctx context.Context, provider string, plain []byte) ([]byte, error) {
	plain = bytes.Trim(plain, " \t\r\n\x00")
	if !json.Valid(plain) {
		log.Warnf(ctx, "%s sealed something other than JSON (%d bytes)", provider, len(plain))
		return nil, refused(fmt.Errorf("%s answered with something that is not JSON", provider))
	}
	return plain, nil
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
