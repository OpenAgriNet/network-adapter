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

// keyBytes is AES-256. A shorter key pasted by mistake is refused rather than
// silently selecting AES-128, which the portal would not open.
const keyBytes = 32

// gcmCodec is the AES-256-GCM wrapping: base64(ciphertext || tag), with a key
// and nonce the portal issues, and the service token in the plaintext.
//
// PM-KISAN's grievance service is the portal it was written for.
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
type gcmCodec struct {
	provider                   string
	keyEnv, nonceEnv, tokenEnv string
	tokenField                 string
	requestField               string
	responsePaths              [][]string
}

// newGCMCodec checks a provider's default-envelope settings.
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
	// Seal appends the 16-byte tag to the ciphertext: exactly the
	// ciphertext || tag layout the portal expects.
	sealed := base64.StdEncoding.EncodeToString(gcm.Seal(nil, nonce, plain, nil))
	return json.Marshal(map[string]string{c.requestField: sealed})
}

// Open decrypts the portal's answer into the plaintext JSON the response half
// reads.
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
		// Where the ciphertext belongs, the portal wrote words: its message to
		// us. Logged clipped, with the token removed in case it was quoted,
		// and never returned -- the caller gets our words, not the portal's.
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
	// The nonce is whatever length the portal issued. Twelve bytes is GCM's
	// standard; any other length is still GCM, with the nonce hashed first,
	// and is what NewGCMWithNonceSize exists for.
	gcm, err := cipher.NewGCMWithNonceSize(block, len(nonce))
	if err != nil {
		return nil, nil, unconfigured(ctx, c.provider, "GCM could not be built: "+err.Error())
	}
	return gcm, nonce, nil
}
