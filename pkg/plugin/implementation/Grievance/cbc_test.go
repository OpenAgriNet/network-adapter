package Grievance

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Produced by Node's crypto the way the legacy PM-KISAN chatbot client does it:
// key string 32 hex characters, AES-128-CBC key and IV both its first 16 UTF-8
// bytes, PKCS#7, base64. Matching them proves this codec speaks the OTP
// service's format byte for byte, not merely that it round-trips with itself.
const (
	cbcKeyString = "0123456789abcdef0123456789abcdef"
	cbcPlain     = `{"Types":"Ben_id","Values":"UP12345678A","Token":"test-otp-token"}`
	cbcCipher    = "MZOLY5yjTYkg9P+xMELaU7hdOIMvjSD5JeV5oUSATtmHsgftQ6GwY8yYERabCGTAZc5bxTopE+wxXWzxwD/e49OKZ5itMwwbCBThrF/svOE="

	// An answer sealed under its own key, named after the '@'.
	cbcReply      = "DladryWQZWg3x/IX77sFz5fyvL8fZS58iyeovMdphFl1xs/uwnFWA3cdIOe8A3wd@fedcba9876543210fedcba9876543210"
	cbcReplyPlain = `{"Rsponce":"True","Message":"OTP Verified"}`

	testOTPTokenEnv = "TEST_PMKISAN_OTP_TOKEN"
	testOTPToken    = "test-otp-token"
)

func otpRealm() realmSettings {
	return realmSettings{kind: envelopeAESCBCKeyInBand, tokenEnv: testOTPTokenEnv, tokenField: "Token",
		requestField: "EncryptedRequest", responsePaths: "d.output,output"}
}

// testCBC is a CBC codec whose key strings are cbcKeyString, so its output is
// predictable.
func testCBC(t *testing.T) *cbcCodec {
	t.Helper()
	c, err := newCBCCodec("pmkisan", otpRealm())
	if err != nil {
		t.Fatalf("newCBCCodec() = %v", err)
	}
	raw, _ := hex.DecodeString(cbcKeyString)
	c.random = bytes.NewReader(bytes.Repeat(raw, 8))
	return c
}

func TestCBCEncryptsWhatTheLegacyClientEncrypts(t *testing.T) {
	got, err := cbcEncrypt(cbcKeyString, []byte(cbcPlain))
	if err != nil {
		t.Fatalf("cbcEncrypt() = %v", err)
	}
	if got != cbcCipher {
		t.Errorf("cbcEncrypt() = %s, want the legacy client's %s", got, cbcCipher)
	}
	raw, _ := base64.StdEncoding.DecodeString(cbcCipher)
	plain, err := cbcDecrypt(cbcKeyString, raw)
	if err != nil || string(plain) != cbcPlain {
		t.Errorf("cbcDecrypt() = %s, %v; want the plaintext back", plain, err)
	}
}

func TestCBCSealSendsTheKeyBesideTheCiphertext(t *testing.T) {
	t.Setenv(testOTPTokenEnv, testOTPToken)
	c := testCBC(t)

	wire, open, err := c.seal(context.Background(), []byte(`{"Types":"Ben_id","Values":"UP12345678A"}`))
	if err != nil {
		t.Fatalf("seal() = %v", err)
	}
	var body map[string]string
	if err := json.Unmarshal(wire, &body); err != nil || len(body) != 1 {
		t.Fatalf("seal() = %s, want exactly {\"EncryptedRequest\": ...}", wire)
	}
	sealed, key, found := strings.Cut(body["EncryptedRequest"], "@")
	if !found || key != cbcKeyString {
		t.Fatalf("EncryptedRequest = %q, want base64(ciphertext)@%s", body["EncryptedRequest"], cbcKeyString)
	}
	raw, _ := base64.StdEncoding.DecodeString(sealed)
	plain, err := cbcDecrypt(key, raw)
	if err != nil {
		t.Fatalf("the sealed request does not decrypt under its own key: %v", err)
	}
	var sent map[string]string
	_ = json.Unmarshal(plain, &sent)
	if sent["Token"] != testOTPToken || sent["Values"] != "UP12345678A" {
		t.Errorf("plaintext = %s, want the mapped fields plus Token", plain)
	}

	// An answer with no key of its own opens under the request's.
	reply, _ := cbcEncrypt(cbcKeyString, []byte(cbcReplyPlain))
	opened, err := open(context.Background(), []byte(`{"d":{"output":"`+reply+`"}}`))
	if err != nil || string(opened) != cbcReplyPlain {
		t.Errorf("open() = %s, %v; want the reply opened under the request's key", opened, err)
	}
}

func TestCBCOpensAnAnswerUnderItsOwnKey(t *testing.T) {
	c := testCBC(t)
	plain, err := c.open(context.Background(), []byte(`{"d":{"output":"`+cbcReply+`"}}`), "wrong-request-key-wrong-request-k")
	if err != nil || string(plain) != cbcReplyPlain {
		t.Fatalf("open() = %s, %v; want the reply opened under the key it names", plain, err)
	}
}

func TestCBCOpenClassifiesWhatItCannotUse(t *testing.T) {
	c := testCBC(t)
	wrongKey, _ := cbcEncrypt("ffffffffffffffffffffffffffffffff", []byte(cbcReplyPlain))
	for _, tc := range []struct {
		name, body, code string
		status           int
	}{
		{"not JSON", `<html>down</html>`, "NET_DOWNSTREAM_UNAVAILABLE", http.StatusBadGateway},
		{"no envelope", `{"d":{}}`, "NET_DOWNSTREAM_UNAVAILABLE", http.StatusBadGateway},
		{"plain text where the ciphertext belongs", `{"d":{"output":"Invalid Token"}}`, "NET_DOWNSTREAM_UNAVAILABLE", http.StatusBadGateway},
		{"sealed under another key", `{"d":{"output":"` + wrongKey + `"}}`, "NET_INTERNAL_ERROR", http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.open(context.Background(), []byte(tc.body), cbcKeyString)
			if err == nil {
				t.Fatal("open() succeeded, want a refusal")
			}
			if code, status := codeOf(t, err); code != tc.code || status != tc.status {
				t.Errorf("got %s/%d, want %s/%d", code, status, tc.code, tc.status)
			}
		})
	}
}

func TestCBCSealFailsClosedWithoutItsToken(t *testing.T) {
	t.Setenv(testOTPTokenEnv, "")
	if _, _, err := testCBC(t).seal(context.Background(), []byte(`{}`)); err == nil {
		t.Fatal("seal() succeeded with the token unset; it must never send without it")
	}
}

func TestNewCBCCodecRefusesIncompleteSettings(t *testing.T) {
	for name, tweak := range map[string]func(*realmSettings){
		"unknown kind":        func(s *realmSettings) { s.kind = "aesGcm" },
		"no request field":    func(s *realmSettings) { s.requestField = "" },
		"no response path":    func(s *realmSettings) { s.responsePaths = "" },
		"token with no field": func(s *realmSettings) { s.tokenField = "" },
	} {
		t.Run(name, func(t *testing.T) {
			s := otpRealm()
			tweak(&s)
			if _, err := newCBCCodec("pmkisan", s); err == nil {
				t.Fatal("newCBCCodec() accepted settings that cannot work")
			}
		})
	}
}
