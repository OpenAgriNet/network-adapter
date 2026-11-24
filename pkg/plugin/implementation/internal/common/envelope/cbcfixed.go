package envelope

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
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

// aesKeyBytes is the key length this codec requires: AES-256.
const aesKeyBytes = 32

// cbcFixedCodec is AES-256-CBC under a key fixed in the environment, with the
// ciphertext written as hex rather than base64.
//
// The IV is whatever the provider says it is, so it is configuration rather
// than a constant: envelopeIvEnv when the provider issues one, and the key's
// own first block when it does not. KCC states the cipher and not the IV rule,
// so this is the knob that settles it without a rebuild (ADR-0001).
type cbcFixedCodec struct {
	provider      string
	keyEnv        string
	ivEnv         string
	tokenEnv      string
	tokenField    string
	requestField  string
	responsePaths [][]string

	// responseOptional passes an answer carrying no envelope to the mapping
	// instead of refusing it. See Open.
	responseOptional bool
}

// newCBCFixedCodec validates the settings. Secrets are read at call time.
func newCBCFixedCodec(provider string, s envelopeSettings) (*cbcFixedCodec, error) {
	missing := missingSettings(map[string]string{
		"envelopeKeyEnv":        s.keyEnv,
		"envelopeRequestField":  s.requestField,
		"envelopeResponsePaths": s.responsePaths,
	})
	missing = checkToken(s.tokenEnv, s.tokenField, missing, "envelope")
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s: envelope %s needs %s", provider, envelopeAESCBCFixedKey, strings.Join(missing, ", "))
	}
	if s.nonceEnv != "" {
		return nil, fmt.Errorf("%s: envelope %s has no nonce; set envelopeIvEnv, or leave it unset to take the IV from the key",
			provider, envelopeAESCBCFixedKey)
	}
	optional, err := boolSetting(provider, "envelopeResponseOptional", s.responseOptional)
	if err != nil {
		return nil, err
	}
	paths, err := parsePaths(provider, "envelopeResponsePaths", s.responsePaths)
	if err != nil {
		return nil, err
	}
	return &cbcFixedCodec{provider: provider, keyEnv: s.keyEnv, ivEnv: s.ivEnv,
		tokenEnv: s.tokenEnv, tokenField: s.tokenField, requestField: s.requestField,
		responsePaths: paths, responseOptional: optional}, nil
}

func (c *cbcFixedCodec) seal(ctx context.Context, mapped []byte) ([]byte, common.Opener, error) {
	sealed, err := c.Seal(ctx, mapped)
	if err != nil {
		return nil, nil, err
	}
	return sealed, c.Open, nil
}

// Seal adds the service token to the mapped request and encrypts it.
func (c *cbcFixedCodec) Seal(ctx context.Context, mapped []byte) ([]byte, error) {
	plain, err := withToken(ctx, c.provider, mapped, c.tokenEnv, c.tokenField)
	if err != nil {
		return nil, err
	}
	block, iv, err := c.cipher(ctx)
	if err != nil {
		return nil, err
	}
	padded := pkcs7Pad(plain, aes.BlockSize)
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
	return json.Marshal(map[string]string{c.requestField: hex.EncodeToString(out)})
}

// Open decrypts the answer. Not JSON: 502. Will not decrypt (our key or IV is
// wrong): 500. Decrypted to something that is not JSON: 502.
//
// An answer that is JSON but carries no envelope is the interesting case. A
// provider that seals only its successes answers a refusal in clear, and
// refusing it here would turn "that OTP is wrong" into "the provider is down"
// before the mapping ever sees the reason. envelopeResponseOptional hands that
// body to the mapping, which is the only place that knows which shapes are
// business answers and which are nonsense.
func (c *cbcFixedCodec) Open(ctx context.Context, answer []byte) ([]byte, error) {
	sealed, found, isJSON := sealedAt(answer, c.responsePaths)
	switch {
	case !isJSON:
		log.Warnf(ctx, "%s answered with a body that is not JSON (%d bytes)", c.provider, len(answer))
		return nil, refused(fmt.Errorf("%s answered without an envelope", c.provider))
	case !found && c.responseOptional:
		log.Infof(ctx, "%s answered with no envelope; passing the body to the mapping to read", c.provider)
		return plainJSON(ctx, c.provider, answer)
	case !found:
		log.Warnf(ctx, "%s answered with no envelope at any configured path", c.provider)
		return nil, refused(fmt.Errorf("%s answered without an envelope", c.provider))
	}

	raw, err := decodeSealedAs(ctx, c.provider, c.tokenEnv, sealed, sealedHex)
	if err != nil {
		return nil, err
	}
	block, iv, err := c.cipher(ctx)
	if err != nil {
		return nil, err
	}
	if len(raw)%aes.BlockSize != 0 {
		log.Warnf(ctx, "%s answered with %d bytes, which is not a whole number of AES blocks", c.provider, len(raw))
		return nil, refused(fmt.Errorf("%s answered without an envelope", c.provider))
	}
	out := make([]byte, len(raw))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, raw)
	plain, err := pkcs7Unpad(out, aes.BlockSize)
	if err != nil {
		log.Errorf(ctx, err, "%s's answer will not decrypt; check %s against the key it issued",
			c.provider, c.keyEnv)
		return nil, model.NewCodedErr(http.StatusInternalServerError, codeInternal,
			fmt.Errorf("the answer from %s could not be decrypted", c.provider))
	}
	return plainJSON(ctx, c.provider, plain)
}

// cipher builds the block cipher and IV from the environment.
func (c *cbcFixedCodec) cipher(ctx context.Context) (cipher.Block, []byte, error) {
	key, err := keyBytes(os.Getenv(c.keyEnv), aesKeyBytes)
	if err != nil {
		return nil, nil, unconfigured(ctx, c.provider, c.keyEnv+" "+err.Error())
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, unconfigured(ctx, c.provider, c.keyEnv+" is not an AES key: "+err.Error())
	}

	// No envelopeIvEnv means the provider derives the IV from the key, the
	// commonest of the fixed-key conventions. Set envelopeIvEnv the moment the
	// provider says otherwise.
	if c.ivEnv == "" {
		return block, key[:aes.BlockSize], nil
	}
	iv, err := keyBytes(os.Getenv(c.ivEnv), aes.BlockSize)
	if err != nil {
		return nil, nil, unconfigured(ctx, c.provider, c.ivEnv+" "+err.Error())
	}
	return block, iv, nil
}

// keyBytes reads want bytes of key material: hex at twice that many
// characters, or the raw bytes at exactly that many. Requiring one length or
// the other leaves no case where the same text could mean two different keys.
func keyBytes(raw string, want int) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	switch len(raw) {
	case 0:
		return nil, fmt.Errorf("is not set")
	case want * 2:
		decoded, err := hex.DecodeString(raw)
		if err != nil {
			return nil, fmt.Errorf("is %d characters, which reads as hex, but is not hex", want*2)
		}
		return decoded, nil
	case want:
		return []byte(raw), nil
	default:
		return nil, fmt.Errorf("must be %d bytes: %d hex characters, or %d plain ones; got %d characters",
			want, want*2, want, len(raw))
	}
}
