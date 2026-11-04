package envelope

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// givenSecrets stands in for a registry row's secret source, recording what it
// was asked for.
type givenSecrets struct {
	values map[string]string
	asked  []string
}

func (g *givenSecrets) Value(_ context.Context, name string) string {
	g.asked = append(g.asked, name)
	return g.values[name]
}

func (g *givenSecrets) Held(name string) string { return g.values[name] }

// emptyEnvironment unsets every secret the codecs could fall back to.
func emptyEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{testKeyEnv, testNonceEnv, testTokenEnv, testOTPTokenEnv} {
		t.Setenv(name, "")
	}
}

func TestGCMSealsWithTheSecretsItWasGiven(t *testing.T) {
	emptyEnvironment(t)
	e := testEnvelope(t)
	e.secrets = &givenSecrets{values: map[string]string{
		testKeyEnv: testKeyHex, testNonceEnv: testNonceHex, testTokenEnv: "given-token"}}

	wire, err := e.Seal(context.Background(), []byte(`{"IdentityNo":"UP12345678A"}`))
	if err != nil {
		t.Fatalf("Seal() = %v", err)
	}
	gcm, nonce, err := e.cipher(context.Background())
	if err != nil {
		t.Fatalf("cipher() = %v", err)
	}
	var body map[string]string
	_ = json.Unmarshal(wire, &body)
	raw, _ := base64.StdEncoding.DecodeString(body["EncryptedRequest"])
	plain, err := gcm.Open(nil, nonce, raw, nil)
	if err != nil {
		t.Fatalf("sealed body does not decrypt with the given key: %v", err)
	}
	var sent map[string]string
	_ = json.Unmarshal(plain, &sent)
	if sent["TokenNo"] != "given-token" {
		t.Errorf("TokenNo = %q, want the given token", sent["TokenNo"])
	}
}

func TestCBCSealsWithTheTokenItWasGiven(t *testing.T) {
	emptyEnvironment(t)
	c := testCBC(t)
	given := &givenSecrets{values: map[string]string{testOTPTokenEnv: "given-otp-token"}}
	c.secrets = given

	if _, _, err := c.seal(context.Background(), []byte(`{"Types":"Ben_id","Values":"UP12345678A"}`)); err != nil {
		t.Fatalf("seal() = %v", err)
	}
	if len(given.asked) != 1 || given.asked[0] != testOTPTokenEnv {
		t.Errorf("asked for %v, want only %s", given.asked, testOTPTokenEnv)
	}
}

func TestRedactTokenHidesATokenHeldInTheGivenSecrets(t *testing.T) {
	emptyEnvironment(t)
	given := &givenSecrets{values: map[string]string{testTokenEnv: "given-token"}}

	got := redactToken("portal said: bad TokenNo given-token", testTokenEnv, given)
	if strings.Contains(got, "given-token") {
		t.Errorf("the given token survived redaction: %s", got)
	}
}

func TestParseGivesTheHooksSecretsToEveryCodec(t *testing.T) {
	t.Parallel()

	given := &givenSecrets{}
	parsed, _, err := Parse(map[string]string{
		"envelope-pmkisan": envelopeAESGCM, "envelopeKeyEnv-pmkisan": testKeyEnv,
		"envelopeNonceEnv-pmkisan": testNonceEnv, "envelopeRequestField-pmkisan": "EncryptedRequest",
		"envelopeResponsePaths-pmkisan": "output",
		"otpEnvelope-pmkisan":           envelopeAESCBCKeyInBand, "otpBindingKey-pmkisan": testOTPBindingKey,
		"otpTokenEnv-pmkisan": testOTPTokenEnv, "otpTokenField-pmkisan": "Token",
		"otpRequestField-pmkisan": "EncryptedRequest", "otpResponsePaths-pmkisan": "output",
		"otpActions-pmkisan": "init",
	}, Hooks{Succeeded: testHooks.Succeeded, Secrets: given})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	e := parsed.Envelopes["pmkisan"].(*providerEnvelope)
	if base := e.base.(*gcmCodec); base.secrets != given {
		t.Error("the base codec was not given the hooks' secrets")
	}
	if otp := e.otp.(*cbcCodec); otp.secrets != given {
		t.Error("the OTP codec was not given the hooks' secrets")
	}
}
