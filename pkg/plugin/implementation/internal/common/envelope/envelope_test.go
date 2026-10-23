package envelope

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
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

// Test material only. Made up for these tests; it is not, and must never be,
// the portal's.
const (
	testKeyHex   = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	testNonceHex = "a0a1a2a3a4a5a6a7a8a9aaab"
	testToken    = "test-service-token"

	testKeyEnv   = "TEST_PMKISAN_KEY"
	testNonceEnv = "TEST_PMKISAN_NONCE"
	testTokenEnv = "TEST_PMKISAN_TOKEN"
)

// legacyVectors were produced by Node's crypto with the same calls the legacy
// PM-KISAN client makes -- createCipheriv("aes-256-gcm", key, iv), then
// base64(ciphertext || authTag) -- over vectorPlaintext. Matching them proves
// this envelope speaks the portal's format byte for byte, not merely that it
// round-trips with itself.
//
// Two nonce lengths, because the portal's is not documented: 12 bytes is GCM's
// standard path, and anything else takes the hashed-nonce path.
const vectorPlaintext = `{"Responce":"True","message":"Grievance lodged"}`

var legacyVectors = map[string]string{
	"a0a1a2a3a4a5a6a7a8a9aaab":         "nTouSDa7bdEBAKXpJS6yqxWOdTL/0jEf/WlDpEWJMnO7EzGewUE2HTPzYK9sHqGEK+AlFb2oL9FNTItRmM+LMQ==",
	"b0b1b2b3b4b5b6b7b8b9babbbcbdbebf": "yi/WF5kjet5MxolK5bGSb88/hP6Daw1FPfZdP2PB+fdYV84BOj0bFh4+sLqc4SkuJLa8xsCECoAGHG2F/cfnRg==",
}

func setTestSecrets(t *testing.T) {
	t.Helper()
	t.Setenv(testKeyEnv, testKeyHex)
	t.Setenv(testNonceEnv, testNonceHex)
	t.Setenv(testTokenEnv, testToken)
}

// pmkisanSettings is PM-KISAN's envelope as config/provider-adapter.yaml
// declares it.
func pmkisanSettings() envelopeSettings {
	return envelopeSettings{
		kind: envelopeAESGCM, keyEnv: testKeyEnv, nonceEnv: testNonceEnv,
		tokenEnv: testTokenEnv, tokenField: "TokenNo",
		requestField: "EncryptedRequest", responsePaths: "d.output,output",
	}
}

func testEnvelope(t *testing.T) *gcmCodec {
	t.Helper()
	e, err := newGCMCodec("pmkisan", pmkisanSettings())
	if err != nil {
		t.Fatalf("newGCMCodec() = %v", err)
	}
	return e
}

// codeOf returns the taxonomy code and status of a coded step error.
func codeOf(t *testing.T, err error) (string, int) {
	t.Helper()
	var coded *model.CodedErr
	if !errors.As(err, &coded) {
		t.Fatalf("error %v (%T) is not a coded error", err, err)
	}
	return coded.BecknError().Code, coded.HTTPStatus()
}

func TestOpenReadsWhatTheLegacyClientWrites(t *testing.T) {
	for nonce, sealed := range legacyVectors {
		t.Run(nonce, func(t *testing.T) {
			setTestSecrets(t)
			t.Setenv(testNonceEnv, nonce)

			plain, err := testEnvelope(t).Open(context.Background(),
				[]byte(`{"d":{"__type":"x","output":"`+sealed+`"}}`))
			if err != nil {
				t.Fatalf("Open() = %v", err)
			}
			if string(plain) != vectorPlaintext {
				t.Errorf("Open() = %s, want %s", plain, vectorPlaintext)
			}
		})
	}
}

func TestSealWritesWhatTheLegacyClientWrites(t *testing.T) {
	// The fixed nonce makes the output deterministic, so a known plaintext
	// must seal to the known ciphertext. TokenNo is added by Seal, so the
	// vector is checked through the cipher directly, then Seal's own output is
	// checked to decrypt to the mapped body plus the token.
	setTestSecrets(t)
	e := testEnvelope(t)

	gcm, nonce, err := e.cipher(context.Background())
	if err != nil {
		t.Fatalf("cipher() = %v", err)
	}
	got := base64.StdEncoding.EncodeToString(gcm.Seal(nil, nonce, []byte(vectorPlaintext), nil))
	if want := legacyVectors[testNonceHex]; got != want {
		t.Errorf("sealed %s, want the legacy client's %s", got, want)
	}

	wire, err := e.Seal(context.Background(), []byte(`{"Type":"Reg_No_Status","IdentityNo":"UP12345678A"}`))
	if err != nil {
		t.Fatalf("Seal() = %v", err)
	}
	var body map[string]string
	if err := json.Unmarshal(wire, &body); err != nil || len(body) != 1 || body["EncryptedRequest"] == "" {
		t.Fatalf("Seal() = %s, want exactly {\"EncryptedRequest\": ...}", wire)
	}
	if strings.Contains(string(wire), testToken) || strings.Contains(string(wire), "UP12345678A") {
		t.Errorf("sealed body %s carries plaintext", wire)
	}
	raw, _ := base64.StdEncoding.DecodeString(body["EncryptedRequest"])
	plain, err := gcm.Open(nil, nonce, raw, nil)
	if err != nil {
		t.Fatalf("Seal()'s output does not decrypt: %v", err)
	}
	var sent map[string]string
	_ = json.Unmarshal(plain, &sent)
	if sent["TokenNo"] != testToken || sent["IdentityNo"] != "UP12345678A" || sent["Type"] != "Reg_No_Status" {
		t.Errorf("plaintext = %s, want the mapped fields plus TokenNo", plain)
	}
}

func TestSealRefusesAMappingThatSetsTheToken(t *testing.T) {
	setTestSecrets(t)
	if _, err := testEnvelope(t).Seal(context.Background(), []byte(`{"TokenNo":"x"}`)); err == nil {
		t.Fatal("Seal() accepted a mapping carrying TokenNo, want a refusal")
	}
}

func TestSealFailsClosedWithoutItsSecrets(t *testing.T) {
	for _, unset := range []string{testKeyEnv, testNonceEnv, testTokenEnv} {
		t.Run(unset, func(t *testing.T) {
			setTestSecrets(t)
			t.Setenv(unset, "")

			_, err := testEnvelope(t).Seal(context.Background(), []byte(`{}`))
			if err == nil {
				t.Fatal("Seal() succeeded with a secret unset; it must never send plaintext")
			}
			code, status := codeOf(t, err)
			if code != "NET_INTERNAL_ERROR" || status != http.StatusInternalServerError {
				t.Errorf("got %s/%d, want NET_INTERNAL_ERROR/500", code, status)
			}
			if strings.Contains(err.Error(), unset) {
				t.Errorf("error %q names the variable; that belongs in the log, not on the wire", err)
			}
		})
	}
}

// The key is hex. Any AES length is taken; anything else is refused.
func TestSealTakesAnyAESKeyLength(t *testing.T) {
	for hexChars, ok := range map[int]bool{32: true, 48: true, 64: true, 20: false, 40: false} {
		t.Run(fmt.Sprintf("%d hex characters", hexChars), func(t *testing.T) {
			setTestSecrets(t)
			t.Setenv(testKeyEnv, strings.Repeat("ab", hexChars/2))
			_, err := testEnvelope(t).Seal(context.Background(), []byte(`{}`))
			if ok && err != nil {
				t.Fatalf("Seal() = %v, want a %d-byte key accepted", err, hexChars/2)
			}
			if !ok && err == nil {
				t.Fatalf("Seal() accepted a %d-byte key, want it refused", hexChars/2)
			}
		})
	}
}

// A 16-byte key seals with AES-128-GCM, readable with the same hex key and
// nonce by any standard implementation.
func TestSealWithASixteenByteKeyIsAES128GCM(t *testing.T) {
	setTestSecrets(t)
	key := testKeyHex[:32]
	t.Setenv(testKeyEnv, key)
	wire, err := testEnvelope(t).Seal(context.Background(), []byte(`{"Type":"Reg_No_Status"}`))
	if err != nil {
		t.Fatalf("Seal() = %v", err)
	}
	var body map[string]string
	if err := json.Unmarshal(wire, &body); err != nil {
		t.Fatalf("sealed body %s: %v", wire, err)
	}
	sealed, _ := base64.StdEncoding.DecodeString(body["EncryptedRequest"])
	rawKey, _ := hex.DecodeString(key)
	nonce, _ := hex.DecodeString(testNonceHex)
	block, _ := aes.NewCipher(rawKey)
	gcm, _ := cipher.NewGCMWithNonceSize(block, len(nonce))
	plain, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		t.Fatalf("AES-128-GCM could not open what Seal() sent: %v", err)
	}
	if !strings.Contains(string(plain), `"Type":"Reg_No_Status"`) || !strings.Contains(string(plain), testToken) {
		t.Errorf("opened %s, want the request with the service token", plain)
	}
}

func TestOpenClassifiesWhatItCannotUse(t *testing.T) {
	setTestSecrets(t)
	e := testEnvelope(t)

	tampered := []byte(legacyVectors[testNonceHex])
	tampered[3] ^= 0x01

	for _, tc := range []struct {
		name, body, code string
		status           int
	}{
		{"not JSON", `<html>Service Unavailable</html>`, "NET_DOWNSTREAM_UNAVAILABLE", 502},
		{"no envelope", `{"d":{}}`, "NET_DOWNSTREAM_UNAVAILABLE", 502},
		{"plain text where the ciphertext belongs", `{"d":{"output":"Invalid Token No."}}`, "NET_DOWNSTREAM_UNAVAILABLE", 502},
		{"an envelope that will not open", `{"d":{"output":"` + string(tampered) + `"}}`, "NET_INTERNAL_ERROR", 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.Open(context.Background(), []byte(tc.body))
			if err == nil {
				t.Fatal("Open() succeeded, want a refusal")
			}
			if code, status := codeOf(t, err); code != tc.code || status != tc.status {
				t.Errorf("got %s/%d, want %s/%d", code, status, tc.code, tc.status)
			}
			if strings.Contains(err.Error(), "Invalid Token") {
				t.Errorf("error %q passes the portal's message through", err)
			}
		})
	}
}

func TestOpenAcceptsABareOutput(t *testing.T) {
	setTestSecrets(t)
	plain, err := testEnvelope(t).Open(context.Background(),
		[]byte(`{"output":"`+legacyVectors[testNonceHex]+`"}`))
	if err != nil || string(plain) != vectorPlaintext {
		t.Fatalf("Open() = %s, %v; want the plaintext from a bare output", plain, err)
	}
}

func TestNewEnvelopeRefusesIncompleteSettings(t *testing.T) {
	for name, tweak := range map[string]func(*envelopeSettings){
		"unknown kind":        func(s *envelopeSettings) { s.kind = "aesCbc" },
		"no key variable":     func(s *envelopeSettings) { s.keyEnv = "" },
		"no nonce variable":   func(s *envelopeSettings) { s.nonceEnv = "" },
		"no request field":    func(s *envelopeSettings) { s.requestField = "" },
		"no response path":    func(s *envelopeSettings) { s.responsePaths = "" },
		"token with no field": func(s *envelopeSettings) { s.tokenField = "" },
		"field with no token": func(s *envelopeSettings) { s.tokenEnv = "" },
		"blank path segment":  func(s *envelopeSettings) { s.responsePaths = "d..output" },
	} {
		t.Run(name, func(t *testing.T) {
			settings := pmkisanSettings()
			tweak(&settings)
			if _, err := newGCMCodec("pmkisan", settings); err == nil {
				t.Fatal("newGCMCodec() accepted settings that cannot work")
			}
		})
	}
}

func TestNewEnvelopeWithoutATokenSendsNone(t *testing.T) {
	setTestSecrets(t)
	settings := pmkisanSettings()
	settings.tokenEnv, settings.tokenField = "", ""
	e, err := newGCMCodec("pmkisan", settings)
	if err != nil {
		t.Fatalf("newGCMCodec() = %v", err)
	}
	wire, err := e.Seal(context.Background(), []byte(`{"a":1}`))
	if err != nil {
		t.Fatalf("Seal() = %v", err)
	}
	var body map[string]string
	_ = json.Unmarshal(wire, &body)
	raw, _ := base64.StdEncoding.DecodeString(body["EncryptedRequest"])
	gcm, nonce, _ := e.cipher(context.Background())
	plain, _ := gcm.Open(nil, nonce, raw, nil)
	if string(plain) != `{"a":1}` {
		t.Errorf("plaintext = %s, want the mapped body with no token added", plain)
	}
}

func TestParseTakesTheEnvelopeOutOfTheProviderBlock(t *testing.T) {
	parsed, rest, err := Parse(map[string]string{
		"bindingKeys":                   "pmkisan|openagrinet:Grievance",
		"authScheme-pmkisan":            "none",
		"envelope-pmkisan":              "aesGcm",
		"envelopeKeyEnv-pmkisan":        testKeyEnv,
		"envelopeNonceEnv-pmkisan":      testNonceEnv,
		"envelopeTokenEnv-pmkisan":      testTokenEnv,
		"envelopeTokenField-pmkisan":    "TokenNo",
		"envelopeRequestField-pmkisan":  "EncryptedRequest",
		"envelopeResponsePaths-pmkisan": "d.output,output",
	}, testHooks)
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	if _, ok := parsed.Envelopes["pmkisan"]; !ok || len(parsed.Envelopes) != 1 {
		t.Errorf("envelopes = %v, want pmkisan's alone", parsed.Envelopes)
	}
	if parsed.HostedElsewhere != nil {
		t.Errorf("HostedElsewhere = %v, want none: pmkisan names no OTP service here", parsed.HostedElsewhere)
	}
	for key := range rest {
		if strings.HasPrefix(key, "envelope") {
			t.Errorf("%s was left for the credential parser, which refuses it", key)
		}
	}
	if rest["authScheme-pmkisan"] != "none" || rest["bindingKeys"] == "" {
		t.Errorf("rest = %v, want everything but the envelope kept", rest)
	}
}

func TestParseWithNoneDeclaredReturnsNone(t *testing.T) {
	parsed, rest, err := Parse(map[string]string{"authScheme-pmfby": "none"}, testHooks)
	if err != nil || parsed.Envelopes != nil || parsed.HostedElsewhere != nil || rest["authScheme-pmfby"] != "none" {
		t.Fatalf("Parse() = %v, %v, %v; want nothing parsed and the config back", parsed, rest, err)
	}
}

// The OTP actions go to the OTP service's participant, named by binding key.
func TestParseNamesTheOTPServicesParticipantForTheOTPActions(t *testing.T) {
	parsed, _, err := Parse(map[string]string{
		"envelope-pmkisan":              "aesGcm",
		"envelopeKeyEnv-pmkisan":        testKeyEnv,
		"envelopeNonceEnv-pmkisan":      testNonceEnv,
		"envelopeRequestField-pmkisan":  "EncryptedRequest",
		"envelopeResponsePaths-pmkisan": "d.output,output",
		"otpEnvelope-pmkisan":           "aesCbcKeyInBand",
		"otpBindingKey-pmkisan":         testOTPBindingKey,
		"otpRequestField-pmkisan":       "EncryptedRequest",
		"otpResponsePaths-pmkisan":      "d.output,output",
		"otpActions-pmkisan":            "init",
	}, testHooks)
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	if got := parsed.HostedElsewhere["pmkisan"]; len(got) != 1 || got["init"] != testOTPBindingKey {
		t.Errorf("HostedElsewhere = %v, want pmkisan's init on %s", parsed.HostedElsewhere, testOTPBindingKey)
	}
}

func TestNewEnvelopeNeedsTheHooksItUses(t *testing.T) {
	if _, err := newProviderEnvelope("pmkisan", pmkisanSettings(), Hooks{}); err == nil {
		t.Error("newProviderEnvelope() took no Succeeded")
	}
	s := pmkisanSettings()
	s.otp, s.otpActions, s.verifyPath, s.verifyActions = otpRealm(), "init", "/v", "support"
	if _, err := newProviderEnvelope("pmkisan", s, Hooks{Succeeded: testHooks.Succeeded}); err == nil {
		t.Error("newProviderEnvelope() took a verify with no Challenge")
	}
}

// Envelope settings without the kind would send the body unsealed.
func TestNewEnvelopeRefusesSettingsWithoutAKind(t *testing.T) {
	s := pmkisanSettings()
	s.kind = ""
	_, err := newProviderEnvelope("pmkisan", s, testHooks)
	if err == nil || !strings.Contains(err.Error(), "envelope is not set") {
		t.Errorf("newProviderEnvelope() = %v, want a refusal naming the missing envelope", err)
	}
}

// A fixed GCM nonce is already a weakness; a short one is refused.
func TestSealRefusesANonceShorterThanGCMs(t *testing.T) {
	setTestSecrets(t)
	t.Setenv(testNonceEnv, "a0a1a2a3")
	_, err := testEnvelope(t).Seal(context.Background(), []byte(`{}`))
	if err == nil {
		t.Fatal("Seal() took a 4-byte nonce")
	}
	if code, _ := codeOf(t, err); code != "NET_INTERNAL_ERROR" {
		t.Errorf("got %s, want NET_INTERNAL_ERROR", code)
	}
}

func TestNewEnvelopeRefusesABadTimeout(t *testing.T) {
	for name, set := range map[string]func(*envelopeSettings){
		"otpVerifyTimeoutMs": func(s *envelopeSettings) { s.verifyTimeoutMs = "soon" },
		"followUpTimeoutMs":  func(s *envelopeSettings) { s.followUpTimeoutMs = "0" },
	} {
		t.Run(name, func(t *testing.T) {
			s := pmkisanSettings()
			set(&s)
			if _, err := newProviderEnvelope("pmkisan", s, testHooks); err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("newProviderEnvelope() = %v, want a refusal naming %s", err, name)
			}
		})
	}
}
