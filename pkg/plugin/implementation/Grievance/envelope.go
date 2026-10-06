package Grievance

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
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

// envelopeAESGCM is the one envelope kind there is: AES-256-GCM with a key and
// nonce the portal issues, carried as base64(ciphertext || tag) inside a
// one-field JSON wrapper. PM-KISAN's grievance API is the portal it was written
// for; the wrapper's field names are configuration, so nothing here names it.
const envelopeAESGCM = "aesGcm"

// keyBytes is AES-256. A shorter key pasted by mistake is refused rather than
// silently selecting AES-128, which the portal would not open.
const keyBytes = 32

// codeInternal is reported when this adapter cannot do its own part.
const codeInternal = "NET_INTERNAL_ERROR"

// envelopeFields are the settings an envelope reads from a provider's block,
// and what each fills. ParseEnvelopes takes these out of the config before
// common.ParseProviderAuth sees it, which would refuse them as unknown
// credential settings.
var envelopeFields = map[string]func(*envelopeSettings, string){
	"envelope":              func(s *envelopeSettings, v string) { s.kind = v },
	"envelopeKeyEnv":        func(s *envelopeSettings, v string) { s.keyEnv = v },
	"envelopeNonceEnv":      func(s *envelopeSettings, v string) { s.nonceEnv = v },
	"envelopeTokenEnv":      func(s *envelopeSettings, v string) { s.tokenEnv = v },
	"envelopeTokenField":    func(s *envelopeSettings, v string) { s.tokenField = v },
	"envelopeRequestField":  func(s *envelopeSettings, v string) { s.requestField = v },
	"envelopeResponsePaths": func(s *envelopeSettings, v string) { s.responsePaths = v },
}

// envelopeSettings is one provider's envelope as written in config.
type envelopeSettings struct {
	kind, keyEnv, nonceEnv, tokenEnv, tokenField, requestField, responsePaths string
}

// ParseEnvelopes reads each provider's envelope out of a plugin's flattened
// settings, and returns the settings with those keys removed.
//
// An operator writes them in the provider's block, beside its auth:
//
//	pmkisan:
//	  authScheme: none
//	  envelope: aesGcm
//	  envelopeKeyEnv: PMKISAN_GRIEVANCE_KEY
//	  envelopeNonceEnv: PMKISAN_GRIEVANCE_NONCE
//	  envelopeTokenEnv: PMKISAN_SERVICE_TOKEN
//	  envelopeTokenField: TokenNo
//	  envelopeRequestField: EncryptedRequest
//	  envelopeResponsePaths: d.output,output
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
		envelope, err := newEnvelope(provider, *s)
		if err != nil {
			return nil, nil, err
		}
		envelopes[provider] = envelope
	}
	return envelopes, rest, nil
}

// envelope is the AES-256-GCM wrapping, and the service token that travels in
// the plaintext.
//
// THE NONCE IS FIXED, and that is the portal's design rather than ours: it
// decrypts with the one nonce it issued and reads none off the wire, so a
// per-request random nonce would be unreadable to it. Reusing a GCM nonce under
// one key is a known weakness -- identical plaintexts produce identical
// ciphertexts, and two ciphertexts together leak their XOR -- and TLS is what
// actually protects the exchange. The nonce is read per call rather than held,
// so the day the portal accepts a nonce per message, the change is here and in
// config, not a redesign.
//
// It holds variable NAMES only. Key, nonce and token are read from the
// environment on every call, so a rotated secret needs no restart, and none of
// them is ever logged or put in an error.
type envelope struct {
	provider                   string
	keyEnv, nonceEnv, tokenEnv string
	tokenField                 string
	requestField               string
	responsePaths              [][]string
}

// The step relies on this at runtime; prove it at compile time.
var _ common.Envelope = (*envelope)(nil)

// newEnvelope checks a provider's envelope settings. Their values are not
// read here: like every credential in this adapter they are read when used.
func newEnvelope(provider string, s envelopeSettings) (*envelope, error) {
	if s.kind != envelopeAESGCM {
		return nil, fmt.Errorf("%s: envelope %q is not one this plugin knows; want %s",
			provider, s.kind, envelopeAESGCM)
	}
	var missing []string
	for setting, value := range map[string]string{
		"envelopeKeyEnv":        s.keyEnv,
		"envelopeNonceEnv":      s.nonceEnv,
		"envelopeRequestField":  s.requestField,
		"envelopeResponsePaths": s.responsePaths,
	} {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, setting)
		}
	}
	// A token is optional, but half of one is a mistake: a name with no
	// variable, or a variable with nowhere to put it.
	if (s.tokenEnv == "") != (s.tokenField == "") {
		missing = append(missing, "both envelopeTokenEnv and envelopeTokenField, or neither")
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("%s: envelope %s needs %s", provider, envelopeAESGCM, strings.Join(missing, ", "))
	}

	e := &envelope{provider: provider, keyEnv: s.keyEnv, nonceEnv: s.nonceEnv,
		tokenEnv: s.tokenEnv, tokenField: s.tokenField, requestField: s.requestField}
	for _, path := range strings.Split(s.responsePaths, ",") {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		segments := strings.Split(path, ".")
		for _, segment := range segments {
			if strings.TrimSpace(segment) == "" {
				return nil, fmt.Errorf("%s: envelopeResponsePaths %q has a blank segment", provider, path)
			}
		}
		e.responsePaths = append(e.responsePaths, segments)
	}
	return e, nil
}

// Seal adds the service token to the mapped request and encrypts it.
//
// The token goes in BEFORE encryption because it is part of the plaintext the
// portal authenticates. It is added here rather than in the mapping because a
// mapping file is published.
func (e *envelope) Seal(ctx context.Context, mapped []byte) ([]byte, error) {
	fields := map[string]any{}
	if len(mapped) > 0 {
		if err := json.Unmarshal(mapped, &fields); err != nil {
			// The request half produced something other than an object, so
			// there is nowhere to put the token. A mapping fault.
			return nil, fmt.Errorf("the %s request mapping must produce a JSON object: %w", e.provider, err)
		}
	}
	if e.tokenField != "" {
		token := os.Getenv(e.tokenEnv)
		if token == "" {
			return nil, e.unconfigured(ctx, e.tokenEnv+" is not set")
		}
		if _, present := fields[e.tokenField]; present {
			// A published mapping setting the token's own key means a
			// credential in a public file. Stopped rather than overwritten.
			return nil, fmt.Errorf("the %s request mapping sets %s, which the envelope supplies; "+
				"remove it from the mapping", e.provider, e.tokenField)
		}
		fields[e.tokenField] = token
	}

	plain, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("the %s request could not be encoded: %w", e.provider, err)
	}
	gcm, nonce, err := e.cipher(ctx)
	if err != nil {
		return nil, err
	}
	// Seal appends the 16-byte tag to the ciphertext: exactly the
	// ciphertext || tag layout the portal expects.
	sealed := base64.StdEncoding.EncodeToString(gcm.Seal(nil, nonce, plain, nil))
	return json.Marshal(map[string]string{e.requestField: sealed})
}

// Open decrypts the portal's answer into the plaintext JSON the reject
// conditions and the response mapping read.
//
// Failures split by whose fault they are:
//
//   - no envelope, or words where the ciphertext belongs: the portal answered
//     with something other than a sealed answer -- in practice a rejection it
//     did not encrypt. 502, NET_DOWNSTREAM_UNAVAILABLE.
//   - an envelope that will not open: our key or nonce has drifted from the
//     portal's. 500, NET_INTERNAL_ERROR -- a 502 would blame a portal that
//     answered correctly.
//   - plaintext that is not JSON: the portal sealed a message, not an answer.
//     502.
//
// Neither the ciphertext nor the plaintext is logged: either may be a record
// about a farmer.
func (e *envelope) Open(ctx context.Context, answer []byte) ([]byte, error) {
	var wire any
	if err := json.Unmarshal(answer, &wire); err != nil {
		log.Warnf(ctx, "%s answered with a body that is not JSON (%d bytes)", e.provider, len(answer))
		return nil, refused(fmt.Errorf("%s answered without an envelope", e.provider))
	}
	sealed, found := e.sealedAnswer(wire)
	if !found {
		log.Warnf(ctx, "%s answered with no envelope at any configured path", e.provider)
		return nil, refused(fmt.Errorf("%s answered without an envelope", e.provider))
	}

	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sealed))
	if err != nil {
		// Where the ciphertext belongs, the portal wrote words: its message to
		// us. Logged clipped, with the token removed in case it was quoted,
		// and never returned -- the caller gets our words, not the portal's.
		log.Warnf(ctx, "%s answered with plain text where its envelope belongs: %s",
			e.provider, e.redact(util.Explain([]byte(sealed))))
		return nil, refused(fmt.Errorf("%s rejected the request", e.provider))
	}

	gcm, nonce, err := e.cipher(ctx)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, raw, nil)
	if err != nil {
		log.Errorf(ctx, err, "%s's answer will not decrypt; check %s and %s against the values it issued",
			e.provider, e.keyEnv, e.nonceEnv)
		return nil, model.NewCodedErr(http.StatusInternalServerError, codeInternal,
			fmt.Errorf("the answer from %s could not be decrypted", e.provider))
	}
	if !json.Valid(plain) {
		log.Warnf(ctx, "%s sealed something other than JSON (%d bytes)", e.provider, len(plain))
		return nil, refused(fmt.Errorf("%s answered with something that is not JSON", e.provider))
	}
	return plain, nil
}

// cipher builds the AEAD from the environment.
func (e *envelope) cipher(ctx context.Context) (cipher.AEAD, []byte, error) {
	rawKey, rawNonce := os.Getenv(e.keyEnv), os.Getenv(e.nonceEnv)
	if rawKey == "" || rawNonce == "" {
		return nil, nil, e.unconfigured(ctx, e.keyEnv+" and "+e.nonceEnv+" must both be set")
	}
	key, err := decodeHex(rawKey)
	if err != nil {
		return nil, nil, e.unconfigured(ctx, e.keyEnv+" is not hex")
	}
	if len(key) != keyBytes {
		return nil, nil, e.unconfigured(ctx, fmt.Sprintf("%s decodes to %d bytes; AES-256 needs %d",
			e.keyEnv, len(key), keyBytes))
	}
	nonce, err := decodeHex(rawNonce)
	if err != nil {
		return nil, nil, e.unconfigured(ctx, e.nonceEnv+" is not hex")
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, e.unconfigured(ctx, "the AES key was refused: "+err.Error())
	}
	// The nonce is whatever length the portal issued. Twelve bytes is GCM's
	// standard; any other length is still GCM, with the nonce hashed first,
	// and is what NewGCMWithNonceSize exists for.
	gcm, err := cipher.NewGCMWithNonceSize(block, len(nonce))
	if err != nil {
		return nil, nil, e.unconfigured(ctx, "GCM could not be built: "+err.Error())
	}
	return gcm, nonce, nil
}

// unconfigured reports envelope configuration that cannot work. The detail --
// which variable -- goes to the log. The wire learns only that this adapter
// could not do its part: a variable name is the operator's business, not a
// network peer's.
func (e *envelope) unconfigured(ctx context.Context, detail string) error {
	err := fmt.Errorf("the envelope for %s is not configured on this adapter", e.provider)
	log.Errorf(ctx, err, "%s", detail)
	return model.NewCodedErr(http.StatusInternalServerError, codeInternal, err)
}

// redact removes the service token from text about to be logged.
func (e *envelope) redact(text string) string {
	if e.tokenEnv == "" {
		return text
	}
	if token := os.Getenv(e.tokenEnv); token != "" {
		text = strings.ReplaceAll(text, token, util.RedactedMarker)
	}
	return text
}

// sealedAnswer finds the sealed string at the first configured path that holds
// one.
func (e *envelope) sealedAnswer(wire any) (string, bool) {
	for _, path := range e.responsePaths {
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
