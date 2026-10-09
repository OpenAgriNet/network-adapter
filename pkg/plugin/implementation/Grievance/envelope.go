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

// The envelope kinds this plugin knows. Each is a portal's wire format; the
// field names inside it are configuration, so nothing here names a portal.
const (
	// AES-256-GCM with a key and nonce the portal issues, carried as
	// base64(ciphertext || tag). See gcm.go.
	envelopeAESGCM = "aesGcm"

	// AES-128-CBC with a key generated per request and sent beside the
	// ciphertext as base64(ciphertext)@key. See cbc.go.
	envelopeAESCBCKeyInBand = "aesCbcKeyInBand"
)

// codeInternal is reported when this adapter cannot do its own part.
const codeInternal = "NET_INTERNAL_ERROR"

// envelopeFields are the settings an envelope reads from a provider's block,
// and what each fills. ParseEnvelopes takes these out of the config before
// common.ParseProviderAuth sees it, which would refuse them as unknown
// credential settings.
//
// Two groups. envelope* is the provider's default wrapping. otp* is a second
// realm some providers keep their OTP on -- another service, another host,
// another cipher, another token -- used by the actions it names, and the
// verification of an OTP before the actions that carry one.
var envelopeFields = map[string]func(*envelopeSettings, string){
	"envelope":              func(s *envelopeSettings, v string) { s.kind = v },
	"envelopeKeyEnv":        func(s *envelopeSettings, v string) { s.keyEnv = v },
	"envelopeNonceEnv":      func(s *envelopeSettings, v string) { s.nonceEnv = v },
	"envelopeTokenEnv":      func(s *envelopeSettings, v string) { s.tokenEnv = v },
	"envelopeTokenField":    func(s *envelopeSettings, v string) { s.tokenField = v },
	"envelopeRequestField":  func(s *envelopeSettings, v string) { s.requestField = v },
	"envelopeResponsePaths": func(s *envelopeSettings, v string) { s.responsePaths = v },

	"otpEnvelope":      func(s *envelopeSettings, v string) { s.otp.kind = v },
	"otpTokenEnv":      func(s *envelopeSettings, v string) { s.otp.tokenEnv = v },
	"otpTokenField":    func(s *envelopeSettings, v string) { s.otp.tokenField = v },
	"otpRequestField":  func(s *envelopeSettings, v string) { s.otp.requestField = v },
	"otpResponsePaths": func(s *envelopeSettings, v string) { s.otp.responsePaths = v },
	"otpActions":       func(s *envelopeSettings, v string) { s.otpActions = v },
	"otpVerifyPath":    func(s *envelopeSettings, v string) { s.verifyPath = v },
	"otpVerifyActions": func(s *envelopeSettings, v string) { s.verifyActions = v },
}

// envelopeSettings is one provider's envelope as written in config.
type envelopeSettings struct {
	kind, keyEnv, nonceEnv, tokenEnv, tokenField, requestField, responsePaths string

	// The OTP realm: its codec, the actions sent through it, and where an OTP
	// is verified before the actions that carry one.
	otp           realmSettings
	otpActions    string
	verifyPath    string
	verifyActions string
}

// realmSettings is a codec's own settings, for a realm that has no fixed key.
type realmSettings struct {
	kind, tokenEnv, tokenField, requestField, responsePaths string
}

// ParseEnvelopes reads each provider's envelope out of a plugin's flattened
// settings, and returns the settings with those keys removed.
//
// An operator writes them in the provider's block, beside its auth:
//
//	pmkisan:
//	  authScheme: none
//	  envelope: aesGcm                     # the grievance service
//	  envelopeKeyEnv: PMKISAN_GRIEVANCE_KEY
//	  envelopeNonceEnv: PMKISAN_GRIEVANCE_NONCE
//	  envelopeTokenEnv: PMKISAN_SERVICE_TOKEN
//	  envelopeTokenField: TokenNo
//	  envelopeRequestField: EncryptedRequest
//	  envelopeResponsePaths: d.output,output
//	  otpEnvelope: aesCbcKeyInBand         # the OTP service
//	  otpTokenEnv: PMKISAN_OTP_TOKEN
//	  otpTokenField: Token
//	  otpRequestField: EncryptedRequest
//	  otpResponsePaths: d.output,output
//	  otpActions: init
//	  otpVerifyPath: /ChatbotOTPVerified
//	  otpVerifyActions: support,status
//
// which pkg/plugin flattens to envelope-pmkisan and friends.
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

// codec is one wire format: it seals a request and returns the opener for the
// answer to that request.
type codec interface {
	seal(ctx context.Context, mapped []byte) ([]byte, common.Opener, error)
}

// providerEnvelope is one provider's wrapping: which codec each action is sent
// through, and whether an action's OTP is verified before it is sent.
type providerEnvelope struct {
	provider string

	// The default codec, for every action not sent through the OTP realm.
	// Nil means those actions go as plain JSON.
	base codec

	// The OTP realm's codec and the actions sent through it.
	otp        codec
	otpActions map[string]bool

	// Verification before the actions that carry an OTP. Nil means none.
	verify *otpVerify
}

// The step relies on this at runtime; prove it at compile time.
var _ common.Envelope = (*providerEnvelope)(nil)

// Seal verifies the action's OTP when it carries one, then wraps the mapped
// request in the action's codec.
//
// The verification runs here, after the request half and before the call, so
// a request the half refused never spends the farmer's OTP, and a wrong OTP
// stops the call it guards: no grievance is lodged or read on a failed check.
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
	return c.seal(ctx, mapped)
}

// newProviderEnvelope checks a provider's envelope settings and builds it.
// Their secret values are not read here: like every credential in this
// adapter they are read when used.
func newProviderEnvelope(provider string, s envelopeSettings) (*providerEnvelope, error) {
	e := &providerEnvelope{provider: provider, otpActions: listSet(s.otpActions)}

	if s.kind != "" {
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
		e.otp = otp
	} else if len(e.otpActions) > 0 || s.otp.tokenEnv != "" || s.otp.requestField != "" || s.otp.responsePaths != "" {
		return nil, fmt.Errorf("%s: otp settings are present but otpEnvelope is not set", provider)
	}

	verifyActions := listSet(s.verifyActions)
	switch {
	case s.verifyPath == "" && len(verifyActions) == 0:
	case s.verifyPath == "" || len(verifyActions) == 0:
		return nil, fmt.Errorf("%s: otpVerifyPath and otpVerifyActions are set together or not at all", provider)
	case e.otp == nil:
		return nil, fmt.Errorf("%s: otpVerifyPath needs otpEnvelope: the OTP is verified on the OTP realm", provider)
	default:
		e.verify = newOTPVerify(provider, s.verifyPath, verifyActions, firstOf(s.otpActions), e.otp.(*cbcCodec))
	}

	if e.base == nil && e.otp == nil {
		return nil, fmt.Errorf("%s: an envelope block needs envelope or otpEnvelope", provider)
	}
	return e, nil
}

// --- shared by the codecs ----------------------------------------------------

// withToken adds a provider's service token to the mapped request, when the
// codec carries one. It goes into the plaintext because that is where the
// portal reads it; it is added here rather than in the mapping because a
// mapping file is published.
func withToken(ctx context.Context, provider string, mapped []byte, tokenEnv, tokenField string) ([]byte, error) {
	fields := map[string]any{}
	if len(mapped) > 0 {
		if err := json.Unmarshal(mapped, &fields); err != nil {
			// The request half produced something other than an object, so
			// there is nowhere to put the token. A mapping fault.
			return nil, fmt.Errorf("the %s request mapping must produce a JSON object: %w", provider, err)
		}
	}
	if tokenField != "" {
		token := os.Getenv(tokenEnv)
		if token == "" {
			return nil, unconfigured(ctx, provider, tokenEnv+" is not set")
		}
		if _, present := fields[tokenField]; present {
			// A published mapping setting the token's own key means a
			// credential in a public file. Stopped rather than overwritten.
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

// checkToken refuses half a token setting: a name with no variable, or a
// variable with nowhere to put it.
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

// unconfigured reports envelope configuration that cannot work. The detail --
// which variable -- goes to the log. The wire learns only that this adapter
// could not do its part: a variable name is the operator's business, not a
// network peer's.
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

// refused classifies an answer the portal gave that cannot be used as one.
func refused(err error) error {
	return model.NewCodedErr(http.StatusBadGateway, util.CodeUpstreamUnavailable, err)
}

// decodeHex reads hex as an operator might paste it: whitespace anywhere,
// including a key wrapped across lines, is dropped.
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

// missingSettings reports the settings left empty, sorted so the message is
// the same every run.
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
