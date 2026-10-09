package Grievance

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
)

// keySeparator joins the ciphertext and its key on the wire: cipher@key.
const keySeparator = "@"

// cbcCodec is the AES-128-CBC wrapping with the key sent in band, as PM-KISAN's
// OTP service reads and writes it:
//
//	key string   32 random hex characters, fresh for every request
//	AES key, IV  both the first 16 BYTES of that string -- its first 16 hex
//	             characters as ASCII -- zero-padded if it were ever shorter
//	padding      PKCS#7
//	request      {"<requestField>": "base64(ciphertext)@<key string>"}
//	answer       "base64(ciphertext)" or "base64(ciphertext)@<reply key>" at a
//	             response path; opened with the reply's key when it names one,
//	             otherwise with the request's
//
// THIS IS OBFUSCATION, NOT ENCRYPTION. The key travels beside the ciphertext,
// in the clear, and the IV is the key, so anyone who can read the request can
// read the plaintext -- exactly who TLS already protects against. And the key
// is 16 ASCII hex digits, 64 bits rather than 128. It is reproduced byte for
// byte because the portal requires it; it is not a security control and must
// not be recorded as one.
//
// The service token is the only secret, and it travels in the plaintext.
type cbcCodec struct {
	provider      string
	tokenEnv      string
	tokenField    string
	requestField  string
	responsePaths [][]string

	// random is where key strings come from. crypto/rand outside tests.
	random io.Reader
}

// newCBCCodec checks the OTP realm's settings.
func newCBCCodec(provider string, s realmSettings) (*cbcCodec, error) {
	if s.kind != envelopeAESCBCKeyInBand {
		return nil, fmt.Errorf("%s: otpEnvelope %q is not one this plugin knows; want %s",
			provider, s.kind, envelopeAESCBCKeyInBand)
	}
	missing := missingSettings(map[string]string{
		"otpRequestField":  s.requestField,
		"otpResponsePaths": s.responsePaths,
	})
	missing = checkToken(s.tokenEnv, s.tokenField, missing, "otp")
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s: otpEnvelope %s needs %s", provider, envelopeAESCBCKeyInBand,
			strings.Join(missing, ", "))
	}
	paths, err := parsePaths(provider, "otpResponsePaths", s.responsePaths)
	if err != nil {
		return nil, err
	}
	return &cbcCodec{provider: provider, tokenEnv: s.tokenEnv, tokenField: s.tokenField,
		requestField: s.requestField, responsePaths: paths, random: rand.Reader}, nil
}

// seal adds the service token, encrypts under a fresh key string, and returns
// the opener that knows that key.
func (c *cbcCodec) seal(ctx context.Context, mapped []byte) ([]byte, common.Opener, error) {
	plain, err := withToken(ctx, c.provider, mapped, c.tokenEnv, c.tokenField)
	if err != nil {
		return nil, nil, err
	}
	keyString, err := c.newKeyString()
	if err != nil {
		return nil, nil, fmt.Errorf("a key for %s could not be generated: %w", c.provider, err)
	}
	sealed, err := cbcEncrypt(keyString, plain)
	if err != nil {
		return nil, nil, fmt.Errorf("the %s request could not be encrypted: %w", c.provider, err)
	}
	body, err := json.Marshal(map[string]string{c.requestField: sealed + keySeparator + keyString})
	if err != nil {
		return nil, nil, err
	}
	return body, func(ctx context.Context, answer []byte) ([]byte, error) {
		return c.open(ctx, answer, keyString)
	}, nil
}

// open decrypts an answer, failing the same three ways the GCM codec does:
// no envelope or words where it belongs (502), a ciphertext that will not open
// (500), plaintext that is not JSON (502).
func (c *cbcCodec) open(ctx context.Context, answer []byte, requestKey string) ([]byte, error) {
	var wire any
	if err := json.Unmarshal(answer, &wire); err != nil {
		log.Warnf(ctx, "%s's OTP service answered with a body that is not JSON (%d bytes)", c.provider, len(answer))
		return nil, refused(fmt.Errorf("%s answered without an envelope", c.provider))
	}
	sealed, found := sealedAnswer(wire, c.responsePaths)
	if !found {
		log.Warnf(ctx, "%s's OTP service answered with no envelope at any configured path", c.provider)
		return nil, refused(fmt.Errorf("%s answered without an envelope", c.provider))
	}

	// base64 has no '@', so the last one, if any, starts the reply's key.
	ciphertext, key := strings.TrimSpace(sealed), requestKey
	if at := strings.LastIndex(ciphertext, keySeparator); at >= 0 {
		if replyKey := ciphertext[at+1:]; replyKey != "" {
			key = replyKey
		}
		ciphertext = ciphertext[:at]
	}
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil || len(raw) == 0 {
		log.Warnf(ctx, "%s's OTP service answered with plain text where its envelope belongs: %s",
			c.provider, redactToken(util.Explain([]byte(sealed)), c.tokenEnv))
		return nil, refused(fmt.Errorf("%s rejected the request", c.provider))
	}

	plain, err := cbcDecrypt(key, raw)
	if err != nil {
		log.Errorf(ctx, err, "%s's OTP service answer will not decrypt", c.provider)
		return nil, model.NewCodedErr(http.StatusInternalServerError, codeInternal,
			fmt.Errorf("the answer from %s could not be decrypted", c.provider))
	}
	plain = bytes.Trim(plain, " \t\r\n\x00")
	if !json.Valid(plain) {
		log.Warnf(ctx, "%s's OTP service sealed something other than JSON (%d bytes)", c.provider, len(plain))
		return nil, refused(fmt.Errorf("%s answered with something that is not JSON", c.provider))
	}
	return plain, nil
}

// newKeyString is 16 random bytes as 32 lower-case hex characters.
func (c *cbcCodec) newKeyString() (string, error) {
	raw := make([]byte, 16)
	if _, err := io.ReadFull(c.random, raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// keyMaterial is the AES key and IV a key string stands for: its first 16
// bytes, zero-padded.
func keyMaterial(keyString string) []byte {
	key := make([]byte, aes.BlockSize)
	copy(key, keyString)
	return key
}

// cbcEncrypt is base64(AES-128-CBC(PKCS#7(plain))) under a key string.
func cbcEncrypt(keyString string, plain []byte) (string, error) {
	key := keyMaterial(keyString)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	padded := pkcs7Pad(plain, aes.BlockSize)
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, key).CryptBlocks(out, padded)
	return base64.StdEncoding.EncodeToString(out), nil
}

// cbcDecrypt reverses cbcEncrypt, refusing a ciphertext or padding that cannot
// be right.
func cbcDecrypt(keyString string, raw []byte) ([]byte, error) {
	if len(raw)%aes.BlockSize != 0 {
		return nil, errors.New("ciphertext is not a whole number of blocks")
	}
	key := keyMaterial(keyString)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(raw))
	cipher.NewCBCDecrypter(block, key).CryptBlocks(out, raw)
	return pkcs7Unpad(out, aes.BlockSize)
}

func pkcs7Pad(data []byte, size int) []byte {
	n := size - len(data)%size
	return append(append([]byte{}, data...), bytes.Repeat([]byte{byte(n)}, n)...)
}

func pkcs7Unpad(data []byte, size int) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("empty plaintext")
	}
	n := int(data[len(data)-1])
	if n == 0 || n > size || n > len(data) {
		return nil, errors.New("bad padding")
	}
	for _, b := range data[len(data)-n:] {
		if int(b) != n {
			return nil, errors.New("bad padding")
		}
	}
	return data[:len(data)-n], nil
}
