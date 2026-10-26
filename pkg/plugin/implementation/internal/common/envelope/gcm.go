package envelope

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
)

// minNonceSize is GCM's standard nonce length. A shorter one is refused; it
// only makes the fixed-nonce break below easier.
const minNonceSize = 12

// gcmCodec is AES-GCM under a fixed key and nonce: base64(ciphertext||tag).
// A fixed nonce breaks GCM; TLS is the only protection (ADR-0001).
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
	// Go appends the 16-byte tag, the layout the provider expects.
	sealed := base64.StdEncoding.EncodeToString(gcm.Seal(nil, nonce, plain, nil))
	return json.Marshal(map[string]string{c.requestField: sealed})
}

// Open decrypts the answer. Missing or plain-text envelope: 502. Will not
// decrypt (our key is wrong): 500. Not JSON: 502. Neither text is logged.
func (c *gcmCodec) Open(ctx context.Context, answer []byte) ([]byte, error) {
	sealed, err := sealedIn(ctx, c.provider, answer, c.responsePaths)
	if err != nil {
		return nil, err
	}
	raw, err := decodeSealed(ctx, c.provider, c.tokenEnv, sealed)
	if err != nil {
		return nil, err
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
	return plainJSON(ctx, c.provider, plain)
}

// cipher builds the AEAD from the environment.
func (c *gcmCodec) cipher(ctx context.Context) (cipher.AEAD, []byte, error) {
	rawKey, rawNonce := os.Getenv(c.keyEnv), os.Getenv(c.nonceEnv)
	if rawKey == "" || rawNonce == "" {
		return nil, nil, unconfigured(ctx, c.provider, c.keyEnv+" and "+c.nonceEnv+" must both be set")
	}
	key, err := hex.DecodeString(strings.TrimSpace(rawKey))
	if err != nil {
		return nil, nil, unconfigured(ctx, c.provider, c.keyEnv+" is not hex")
	}
	nonce, err := hex.DecodeString(strings.TrimSpace(rawNonce))
	if err != nil {
		return nil, nil, unconfigured(ctx, c.provider, c.nonceEnv+" is not hex")
	}
	if len(nonce) < minNonceSize {
		return nil, nil, unconfigured(ctx, c.provider, fmt.Sprintf("%s decodes to %d bytes; GCM needs at least %d",
			c.nonceEnv, len(nonce), minNonceSize))
	}

	block, err := aes.NewCipher(key) // refuses a key that is not 16, 24 or 32 bytes
	if err != nil {
		return nil, nil, unconfigured(ctx, c.provider, c.keyEnv+" is not an AES key: "+err.Error())
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, len(nonce))
	if err != nil {
		return nil, nil, unconfigured(ctx, c.provider, "GCM could not be built: "+err.Error())
	}
	return gcm, nonce, nil
}
