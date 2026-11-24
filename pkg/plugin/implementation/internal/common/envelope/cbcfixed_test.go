package envelope

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

// Produced by an AES-256 written from the FIPS-197 tables, not by crypto/aes:
// key the 32 bytes 00..1f, IV its first 16, PKCS#7, ciphertext as hex. Matching
// it proves the codec implements the format KCC describes rather than merely
// round-tripping with itself.
const (
	fixedKeyHex    = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	fixedKeyPlain  = `{"mobileNumber":"9876543210","otpFor":"krishika"}`
	fixedKeyCipher = "bb1bff3c24963e66418ec40ed07744015b7236bebdc2b48b4b978c7d6b0b699f" +
		"2aeafc1b3637c4dc18a0f287cb788dae202ec3173cf937ad6b3d732567278acb"

	testKCCKeyEnv = "TEST_KCC_ENCRYPTION_KEY"
	testKCCIvEnv  = "TEST_KCC_IV"
)

func kccSettings() envelopeSettings {
	return envelopeSettings{kind: envelopeAESCBCFixedKey, keyEnv: testKCCKeyEnv,
		requestField: "data", responsePaths: "data"}
}

func testFixed(t *testing.T, s envelopeSettings) *cbcFixedCodec {
	t.Helper()
	t.Setenv(testKCCKeyEnv, fixedKeyHex)
	c, err := newCBCFixedCodec("kcc", s)
	if err != nil {
		t.Fatalf("newCBCFixedCodec() = %v", err)
	}
	return c
}

// sealedField is the hex string the codec put at requestField.
func sealedField(t *testing.T, body []byte) string {
	t.Helper()
	var wire map[string]string
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("the sealed body is not a JSON object of strings: %v", err)
	}
	return wire["data"]
}

func TestFixedKeySealsWhatKCCDescribes(t *testing.T) {
	c := testFixed(t, kccSettings())
	body, err := c.Seal(context.Background(), []byte(fixedKeyPlain))
	if err != nil {
		t.Fatalf("Seal() = %v", err)
	}
	if got := sealedField(t, body); got != fixedKeyCipher {
		t.Errorf("Seal() sealed\n%s\nwant\n%s", got, fixedKeyCipher)
	}
}

func TestFixedKeyWritesHexNotBase64(t *testing.T) {
	c := testFixed(t, kccSettings())
	body, err := c.Seal(context.Background(), []byte(fixedKeyPlain))
	if err != nil {
		t.Fatalf("Seal() = %v", err)
	}
	sealed := sealedField(t, body)
	for _, r := range sealed {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("Seal() wrote %q, which is not lower-case hex", sealed)
		}
	}
}

func TestFixedKeyOpensWhatItSeals(t *testing.T) {
	c := testFixed(t, kccSettings())
	answer := []byte(`{"status":true,"data":"` + fixedKeyCipher + `"}`)
	plain, err := c.Open(context.Background(), answer)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	if string(plain) != fixedKeyPlain {
		t.Errorf("Open() = %s, want %s", plain, fixedKeyPlain)
	}
}

// The wrapper is dropped: a mapping reads response.mobileNumber, not
// response.data.mobileNumber.
func TestFixedKeyOpenReplacesTheWholeBody(t *testing.T) {
	c := testFixed(t, kccSettings())
	plain, err := c.Open(context.Background(), []byte(`{"status":true,"data":"`+fixedKeyCipher+`"}`))
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(plain, &fields); err != nil {
		t.Fatalf("Open() did not return JSON: %v", err)
	}
	if _, present := fields["status"]; present {
		t.Error("Open() kept the provider's wrapper; mappings would have to read response.data.*")
	}
	if fields["mobileNumber"] != "9876543210" {
		t.Errorf("Open() = %v, want the decrypted fields at the top level", fields)
	}
}

func TestFixedKeyTakesTheKeyAsHexOrAsPlainBytes(t *testing.T) {
	// 64 hex characters and 32 plain ones are both 32 bytes of key material,
	// and no string can be read as either.
	for _, key := range []string{fixedKeyHex, "0123456789abcdef0123456789abcdef"} {
		t.Setenv(testKCCKeyEnv, key)
		c, err := newCBCFixedCodec("kcc", kccSettings())
		if err != nil {
			t.Fatalf("newCBCFixedCodec() = %v", err)
		}
		if _, err := c.Seal(context.Background(), []byte(`{"a":"b"}`)); err != nil {
			t.Errorf("Seal() with key %q = %v", key, err)
		}
	}
}

func TestFixedKeyRefusesAKeyOfTheWrongLength(t *testing.T) {
	for _, key := range []string{"", "short", strings.Repeat("a", 31), strings.Repeat("zz", 32)} {
		t.Setenv(testKCCKeyEnv, key)
		c, err := newCBCFixedCodec("kcc", kccSettings())
		if err != nil {
			t.Fatalf("newCBCFixedCodec() = %v", err)
		}
		_, err = c.Seal(context.Background(), []byte(`{"a":"b"}`))
		if err == nil {
			t.Errorf("Seal() with key %q = nil, want a configuration error", key)
			continue
		}
		var coded *model.CodedErr
		if !errors.As(err, &coded) || coded.HTTPStatus() != http.StatusInternalServerError {
			t.Errorf("Seal() with key %q = %v, want a 500", key, err)
		}
	}
}

func TestFixedKeyTakesTheIVFromTheKeyUnlessToldOtherwise(t *testing.T) {
	plain := []byte(`{"a":"b"}`)

	c := testFixed(t, kccSettings())
	fromKey, err := c.Seal(context.Background(), plain)
	if err != nil {
		t.Fatalf("Seal() = %v", err)
	}

	// The same IV, named explicitly, must seal to the same bytes.
	s := kccSettings()
	s.ivEnv = testKCCIvEnv
	t.Setenv(testKCCIvEnv, fixedKeyHex[:32]) // the key's first 16 bytes
	same := testFixed(t, s)
	explicit, err := same.Seal(context.Background(), plain)
	if err != nil {
		t.Fatalf("Seal() with envelopeIvEnv = %v", err)
	}
	if sealedField(t, fromKey) != sealedField(t, explicit) {
		t.Error("the default IV is not the key's first block")
	}

	// A different IV must seal to different bytes, or envelopeIvEnv is ignored.
	t.Setenv(testKCCIvEnv, strings.Repeat("ff", 16))
	other := testFixed(t, s)
	differs, err := other.Seal(context.Background(), plain)
	if err != nil {
		t.Fatalf("Seal() with a different IV = %v", err)
	}
	if sealedField(t, differs) == sealedField(t, fromKey) {
		t.Error("envelopeIvEnv was ignored")
	}
}

// The reason envelopeResponseOptional exists: a provider that seals only its
// successes must still be able to say "that OTP is wrong".
func TestFixedKeyPassesAnUnsealedRefusalToTheMapping(t *testing.T) {
	refusal := []byte(`{"status":false,"message":"Invalid OTP"}`)

	s := kccSettings()
	s.responseOptional = "true"
	open := testFixed(t, s)
	plain, err := open.Open(context.Background(), refusal)
	if err != nil {
		t.Fatalf("Open() = %v, want the body handed to the mapping", err)
	}
	if string(plain) != string(refusal) {
		t.Errorf("Open() = %s, want the body unchanged", plain)
	}

	// Off by default, so no provider configured today changes behaviour.
	shut := testFixed(t, kccSettings())
	if _, err := shut.Open(context.Background(), refusal); err == nil {
		t.Fatal("Open() = nil without envelopeResponseOptional, want a 502")
	}
}

func TestFixedKeyRefusalsAreTheRightStatus(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		answer string
		status int
	}{
		{"not JSON at all", `<html>502 Bad Gateway</html>`, http.StatusBadGateway},
		{"JSON without the envelope", `{"status":false}`, http.StatusBadGateway},
		{"the envelope is not hex", `{"data":"not-hex-at-all"}`, http.StatusBadGateway},
		{"a hex envelope that is not whole blocks", `{"data":"abcdef"}`, http.StatusBadGateway},
		{"hex that will not decrypt under our key", `{"data":"` + strings.Repeat("ab", 32) + `"}`, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := testFixed(t, kccSettings())
			_, err := c.Open(ctx, []byte(tc.answer))
			if err == nil {
				t.Fatalf("Open(%s) = nil, want an error", tc.answer)
			}
			var coded *model.CodedErr
			if !errors.As(err, &coded) || coded.HTTPStatus() != tc.status {
				t.Errorf("Open(%s) = %v, want status %d", tc.answer, err, tc.status)
			}
		})
	}
}

// A truncated success must not be mistaken for a refusal: responseOptional
// only forgives a missing envelope, never a broken one.
func TestFixedKeyOptionalDoesNotForgiveABrokenEnvelope(t *testing.T) {
	s := kccSettings()
	s.responseOptional = "true"
	c := testFixed(t, s)
	if _, err := c.Open(context.Background(), []byte(`{"data":"abcdef"}`)); err == nil {
		t.Error("Open() = nil for a truncated envelope, want it refused")
	}
}

func TestFixedKeySettingsAreValidated(t *testing.T) {
	cases := map[string]func(*envelopeSettings){
		"no envelopeKeyEnv":                   func(s *envelopeSettings) { s.keyEnv = "" },
		"no envelopeRequestField":             func(s *envelopeSettings) { s.requestField = "" },
		"no envelopeResponsePaths":            func(s *envelopeSettings) { s.responsePaths = "" },
		"a nonce, which CBC has not":          func(s *envelopeSettings) { s.nonceEnv = "KCC_NONCE" },
		"half a token":                        func(s *envelopeSettings) { s.tokenEnv = "KCC_TOKEN" },
		"responseOptional that is not a bool": func(s *envelopeSettings) { s.responseOptional = "yes please" },
	}
	for name, break_ := range cases {
		t.Run(name, func(t *testing.T) {
			s := kccSettings()
			break_(&s)
			if _, err := newCBCFixedCodec("kcc", s); err == nil {
				t.Errorf("newCBCFixedCodec() = nil, want a startup error for %s", name)
			}
		})
	}
}

// Set on a codec that cannot read them, these must fail loudly at startup
// rather than be silently ignored.
func TestFixedKeyOnlySettingsAreRefusedOnOtherCodecs(t *testing.T) {
	for _, kind := range []string{envelopeAESGCM, envelopeAESCBCKeyInBand} {
		for _, broken := range []envelopeSettings{
			{kind: kind, ivEnv: "KCC_IV"},
			{kind: kind, responseOptional: "true"},
		} {
			broken.requestField, broken.responsePaths = "data", "data"
			if _, err := parseBaseCodec("kcc", broken); err == nil {
				t.Errorf("parseBaseCodec(%s) = nil, want envelope-only settings refused", kind)
			}
		}
	}
}

func TestFixedKeyIsReachableThroughParse(t *testing.T) {
	c, err := parseBaseCodec("kcc", kccSettings())
	if err != nil {
		t.Fatalf("parseBaseCodec() = %v", err)
	}
	if _, ok := c.(*cbcFixedCodec); !ok {
		t.Errorf("parseBaseCodec() = %T, want *cbcFixedCodec", c)
	}
}
