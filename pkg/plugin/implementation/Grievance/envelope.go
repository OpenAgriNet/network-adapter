package Grievance

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
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

	"otpEnvelope":      func(s *envelopeSettings, v string) { s.otp.kind = v },
	"otpBaseUrlEnv":    func(s *envelopeSettings, v string) { s.otp.baseURLEnv = v },
	"otpTokenEnv":      func(s *envelopeSettings, v string) { s.otp.tokenEnv = v },
	"otpTokenField":    func(s *envelopeSettings, v string) { s.otp.tokenField = v },
	"otpRequestField":  func(s *envelopeSettings, v string) { s.otp.requestField = v },
	"otpResponsePaths": func(s *envelopeSettings, v string) { s.otp.responsePaths = v },
	"otpActions":       func(s *envelopeSettings, v string) { s.otpActions = v },
	"otpVerifyPath":    func(s *envelopeSettings, v string) { s.verifyPath = v },
	"otpVerifyActions": func(s *envelopeSettings, v string) { s.verifyActions = v },

	"followUpPath":    func(s *envelopeSettings, v string) { s.followUpPath = v },
	"followUpField":   func(s *envelopeSettings, v string) { s.followUpField = v },
	"followUpActions": func(s *envelopeSettings, v string) { s.followUpActions = v },
}

// envelopeSettings is one provider's envelope as written in config.
type envelopeSettings struct {
	kind, keyEnv, nonceEnv, tokenEnv, tokenField, requestField, responsePaths string

	otp           realmSettings
	otpActions    string
	verifyPath    string
	verifyActions string

	followUpPath, followUpField, followUpActions string
}

// realmSettings configures a CBC codec. baseURLEnv names the variable holding
// the OTP service's host, which differs from the participant's.
type realmSettings struct {
	kind, baseURLEnv, tokenEnv, tokenField, requestField, responsePaths string
}

// ParseEnvelopes reads each provider's envelope settings (flattened as
// "setting-provider") and returns the remaining settings. See the pmkisan and
// pmkisan-chatbot blocks in config/provider-adapter.yaml.
func ParseEnvelopes(config map[string]string) (map[string]common.Envelope, map[string]string, error) {
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
		return nil, rest, nil
	}

	envelopes := make(map[string]common.Envelope, len(settings))
	for provider, s := range settings {
		envelope, err := newProviderEnvelope(provider, *s)
		if err != nil {
			return nil, nil, err
		}
		envelopes[provider] = envelope
	}
	return envelopes, rest, nil
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
	otpBaseURLEnv string
	otpActions    map[string]bool
	verify        *otpVerify
	followUp      *followUp
}

var (
	_ common.Envelope = (*providerEnvelope)(nil)
	_ common.Router   = (*providerEnvelope)(nil)
)

// BaseURLFor sends OTP actions to the OTP host, everything else to the participant.
func (e *providerEnvelope) BaseURLFor(ctx context.Context, action, participant string) (string, error) {
	if !e.otpActions[action] {
		return participant, nil
	}
	return e.otpHost(ctx)
}

// otpHost reads the OTP host from the environment. Unset fails the call; there
// is no fallback to the participant's host.
func (e *providerEnvelope) otpHost(ctx context.Context) (string, error) {
	host := strings.TrimSpace(os.Getenv(e.otpBaseURLEnv))
	if host == "" {
		return "", unconfigured(ctx, e.provider, e.otpBaseURLEnv+" is not set")
	}
	return host, nil
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
	participant := ""
	if exchange.Plan != nil {
		participant = exchange.Plan.BaseURL
	}
	base, err := e.BaseURLFor(ctx, exchange.Action, participant)
	if err != nil {
		return nil, nil, err
	}
	return sealed, e.followUp.wrap(c, exchange, base, mapped, open), nil
}

// newProviderEnvelope validates the settings. Secrets are read at call time.
func newProviderEnvelope(provider string, s envelopeSettings) (*providerEnvelope, error) {
	e := &providerEnvelope{provider: provider, otpBaseURLEnv: s.otp.baseURLEnv, otpActions: listSet(s.otpActions)}

	switch s.kind {
	case "":
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
		if s.otp.baseURLEnv == "" {
			return nil, fmt.Errorf("%s: otpEnvelope needs otpBaseUrlEnv, the variable holding the OTP service's host", provider)
		}
		e.otp = otp
	} else if len(e.otpActions) > 0 || s.otp.baseURLEnv != "" || s.otp.tokenEnv != "" || s.otp.requestField != "" || s.otp.responsePaths != "" {
		return nil, fmt.Errorf("%s: otp settings are present but otpEnvelope is not set", provider)
	}

	verifyActions := listSet(s.verifyActions)
	switch {
	case s.verifyPath == "" && len(verifyActions) == 0:
	case s.verifyPath == "" || len(verifyActions) == 0:
		return nil, fmt.Errorf("%s: otpVerifyPath and otpVerifyActions are set together or not at all", provider)
	case e.otp != nil:
		// Separate OTP service: verify there.
		e.verify = newOTPVerify(provider, s.verifyPath, verifyActions, firstOf(s.otpActions), e.otp.(*cbcCodec),
			func(ctx context.Context, _ common.Exchange) (string, error) { return e.otpHost(ctx) })
	default:
		// The participant is the OTP service: verify on its host.
		cbc, onOTPService := e.base.(*cbcCodec)
		if !onOTPService {
			return nil, fmt.Errorf("%s: otpVerifyPath needs otpEnvelope, or envelope %s: the OTP is verified on the OTP service",
				provider, envelopeAESCBCKeyInBand)
		}
		e.verify = newOTPVerify(provider, s.verifyPath, verifyActions, "", cbc, participantHost)
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
		e.followUp = newFollowUp(provider, s.followUpPath, s.followUpField, followUpActions)
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

// refused is a 502: the portal's answer is unusable.
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

// firstOf returns the first entry of a comma-separated list.
func firstOf(raw string) string {
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			return trimmed
		}
	}
	return ""
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
