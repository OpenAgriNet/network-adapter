package Grievance

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
)

// keyBytes is AES-256; shorter keys are refused.
const keyBytes = 32

// gcmCodec is PM-KISAN grievance's AES-256-GCM envelope: base64(ciphertext||tag).
// The nonce is fixed because the portal requires it (a known GCM weakness; TLS
// protects the exchange). Key, nonce and token are env var names, read per call.
type gcmCodec struct {
	provider                   string
	keyEnv, nonceEnv, tokenEnv string
	tokenField                 string
	requestField               string
	responsePaths              [][]string
}

// newGCMCodec validates the settings.
func newGCMCodec(provider string, s envelopeSettings) (*gcmCodec, error) {
	if s.kind != envelopeAESGCM {
		return nil, fmt.Errorf("%s: envelope %q is not one this plugin knows; want %s",
			provider, s.kind, envelopeAESGCM)
	}
	missing := missingSettings(map[string]string{
		"envelopeKeyEnv":        s.keyEnv,
		"envelopeNonceEnv":      s.nonceEnv,
		"envelopeRequestField":  s.requestField,
		"envelopeResponsePaths": s.responsePaths,
	})
	missing = checkToken(s.tokenEnv, s.tokenField, missing, "envelope")
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s: envelope %s needs %s", provider, envelopeAESGCM, strings.Join(missing, ", "))
	}
	paths, err := parsePaths(provider, "envelopeResponsePaths", s.responsePaths)
	if err != nil {
		return nil, err
	}
	return &gcmCodec{provider: provider, keyEnv: s.keyEnv, nonceEnv: s.nonceEnv,
		tokenEnv: s.tokenEnv, tokenField: s.tokenField, requestField: s.requestField,
		responsePaths: paths}, nil
}

func (c *gcmCodec) seal(ctx context.Context, mapped []byte) ([]byte, common.Opener, error) {
	sealed, err := c.Seal(ctx, mapped)
	if err != nil {
		return nil, nil, err
	}
	return sealed, c.Open, nil
}

// Seal adds the service token to the mapped request and encrypts it.
func (c *gcmCodec) Seal(ctx context.Context, mapped []byte) ([]byte, error) {
	plain, err := withToken(ctx, c.provider, mapped, c.tokenEnv, c.tokenField)
	if err != nil {
		return nil, err
	}
	gcm, nonce, err := c.cipher(ctx)
	if err != nil {
		return nil, err
	}
	// Go appends the 16-byte tag, the layout the portal expects.
	sealed := base64.StdEncoding.EncodeToString(gcm.Seal(nil, nonce, plain, nil))
	return json.Marshal(map[string]string{c.requestField: sealed})
}

// Open decrypts the answer. Missing or plain-text envelope: 502. Will not
// decrypt (our key is wrong): 500. Not JSON: 502. Neither text is logged.
func (c *gcmCodec) Open(ctx context.Context, answer []byte) ([]byte, error) {
	var wire any
	if err := json.Unmarshal(answer, &wire); err != nil {
		log.Warnf(ctx, "%s answered with a body that is not JSON (%d bytes)", c.provider, len(answer))
		return nil, refused(fmt.Errorf("%s answered without an envelope", c.provider))
	}
	sealed, found := sealedAnswer(wire, c.responsePaths)
	if !found {
		log.Warnf(ctx, "%s answered with no envelope at any configured path", c.provider)
		return nil, refused(fmt.Errorf("%s answered without an envelope", c.provider))
	}

	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sealed))
	if err != nil {
		// A plain-text rejection: log it redacted, never return it.
		log.Warnf(ctx, "%s answered with plain text where its envelope belongs: %s",
			c.provider, redactToken(util.Explain([]byte(sealed)), c.tokenEnv))
		return nil, refused(fmt.Errorf("%s rejected the request", c.provider))
	}

	gcm, nonce, err := c.cipher(ctx)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, raw, nil)
	if err != nil {
		log.Errorf(ctx, err, "%s's answer will not decrypt; check %s and %s against the values it issued",
			c.provider, c.keyEnv, c.nonceEnv)
		return nil, model.NewCodedErr(http.StatusInternalServerError, codeInternal,
			fmt.Errorf("the answer from %s could not be decrypted", c.provider))
	}
	if !json.Valid(plain) {
		log.Warnf(ctx, "%s sealed something other than JSON (%d bytes)", c.provider, len(plain))
		return nil, refused(fmt.Errorf("%s answered with something that is not JSON", c.provider))
	}
	return plain, nil
}

// cipher builds the AEAD from the environment.
func (c *gcmCodec) cipher(ctx context.Context) (cipher.AEAD, []byte, error) {
	rawKey, rawNonce := os.Getenv(c.keyEnv), os.Getenv(c.nonceEnv)
	if rawKey == "" || rawNonce == "" {
		return nil, nil, unconfigured(ctx, c.provider, c.keyEnv+" and "+c.nonceEnv+" must both be set")
	}
	key, err := decodeHex(rawKey)
	if err != nil {
		return nil, nil, unconfigured(ctx, c.provider, c.keyEnv+" is not hex")
	}
	if len(key) != keyBytes {
		return nil, nil, unconfigured(ctx, c.provider, fmt.Sprintf("%s decodes to %d bytes; AES-256 needs %d",
			c.keyEnv, len(key), keyBytes))
	}
	nonce, err := decodeHex(rawNonce)
	if err != nil {
		return nil, nil, unconfigured(ctx, c.provider, c.nonceEnv+" is not hex")
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, unconfigured(ctx, c.provider, "the AES key was refused: "+err.Error())
	}
	// Any nonce length the portal issued.
	gcm, err := cipher.NewGCMWithNonceSize(block, len(nonce))
	if err != nil {
		return nil, nil, unconfigured(ctx, c.provider, "GCM could not be built: "+err.Error())
	}
	return gcm, nonce, nil
}
