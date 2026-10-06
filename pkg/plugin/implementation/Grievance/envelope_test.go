package Grievance

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

func testEnvelope(t *testing.T) *envelope {
	t.Helper()
	e, err := newEnvelope("pmkisan", pmkisanSettings())
	if err != nil {
		t.Fatalf("newEnvelope() = %v", err)
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

func TestSealRefusesAKeyThatIsNotAES256(t *testing.T) {
	setTestSecrets(t)
	t.Setenv(testKeyEnv, testKeyHex[:32]) // 16 bytes: AES-128
	if _, err := testEnvelope(t).Seal(context.Background(), []byte(`{}`)); err == nil {
		t.Fatal("Seal() accepted a 16-byte key, want it refused")
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
			if _, err := newEnvelope("pmkisan", settings); err == nil {
				t.Fatal("newEnvelope() accepted settings that cannot work")
			}
		})
	}
}

func TestNewEnvelopeWithoutATokenSendsNone(t *testing.T) {
	setTestSecrets(t)
	settings := pmkisanSettings()
	settings.tokenEnv, settings.tokenField = "", ""
	e, err := newEnvelope("pmkisan", settings)
	if err != nil {
		t.Fatalf("newEnvelope() = %v", err)
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

func TestParseEnvelopesTakesTheEnvelopeOutOfTheProviderBlock(t *testing.T) {
	envelopes, rest, err := ParseEnvelopes(map[string]string{
		"bindingKeys":                   "pmkisan|openagrinet:PMKISANGrievance",
		"authScheme-pmkisan":            "none",
		"envelope-pmkisan":              "aesGcm",
		"envelopeKeyEnv-pmkisan":        testKeyEnv,
		"envelopeNonceEnv-pmkisan":      testNonceEnv,
		"envelopeTokenEnv-pmkisan":      testTokenEnv,
		"envelopeTokenField-pmkisan":    "TokenNo",
		"envelopeRequestField-pmkisan":  "EncryptedRequest",
		"envelopeResponsePaths-pmkisan": "d.output,output",
	})
	if err != nil {
		t.Fatalf("ParseEnvelopes() = %v", err)
	}
	if _, ok := envelopes["pmkisan"]; !ok || len(envelopes) != 1 {
		t.Errorf("envelopes = %v, want pmkisan's alone", envelopes)
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

func TestParseEnvelopesWithNoneDeclaredReturnsNone(t *testing.T) {
	envelopes, rest, err := ParseEnvelopes(map[string]string{"authScheme-pmfby": "none"})
	if err != nil || envelopes != nil || rest["authScheme-pmfby"] != "none" {
		t.Fatalf("ParseEnvelopes() = %v, %v, %v; want nil and the config back", envelopes, rest, err)
	}
}
