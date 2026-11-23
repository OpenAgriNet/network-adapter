package common

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
)

func bodyProfile() AuthProfile {
	return AuthProfile{Scheme: util.AuthSchemeBody, BodyField: "apiKey", BodyValueEnv: "TEST_SATHI_KEY"}
}

func bodyAuthStep(t *testing.T, method string, mapped string, sent *map[string]any, calls *int) *Step {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		if err := json.NewDecoder(r.Body).Decode(sent); err != nil {
			t.Errorf("provider could not read the body as JSON: %v", err)
		}
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(upstream.Close)

	mapper := &stubMapper{requestResult: []byte(mapped), responseResult: []byte(`{}`)}
	return newStep(t, &stubRegistry{plan: testPlan(upstream.URL, method)}, mapper,
		func(c *Config) { c.setProviderAuth(bodyProfile()) })
}

func TestRunSendsTheCredentialAsABodyField(t *testing.T) {
	t.Setenv("TEST_SATHI_KEY", "s3cr3t")

	var sent map[string]any
	var calls int
	step := bodyAuthStep(t, http.MethodPost, `{"cropCode":"A0102","districtCode":"490"}`, &sent, &calls)

	if _, err := runStep(t, step, selectBody); err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}
	if sent["apiKey"] != "s3cr3t" {
		t.Errorf("apiKey = %v, want the value from the environment", sent["apiKey"])
	}
	if sent["cropCode"] != "A0102" || sent["districtCode"] != "490" {
		t.Errorf("sent = %v, want the mapped fields kept beside the credential", sent)
	}
}

func TestRunRefusesABodyCredentialThatIsNotSet(t *testing.T) {
	t.Setenv("TEST_SATHI_KEY", "")

	var sent map[string]any
	var calls int
	step := bodyAuthStep(t, http.MethodPost, `{"cropCode":"A0102"}`, &sent, &calls)

	_, err := runStep(t, step, selectBody)
	if err == nil {
		t.Fatal("expected an unset credential to be refused")
	}
	if calls != 0 {
		t.Errorf("provider was called %d times, want none without a credential", calls)
	}
	if strings.Contains(err.Error(), "TEST_SATHI_KEY") {
		t.Errorf("error %q names the environment variable on the wire", err)
	}
}

func TestRunRefusesABodyCredentialWhenTheMappedBodyIsNotAnObject(t *testing.T) {
	t.Setenv("TEST_SATHI_KEY", "s3cr3t")

	var sent map[string]any
	var calls int
	step := bodyAuthStep(t, http.MethodPost, `["A0102"]`, &sent, &calls)

	_, err := runStep(t, step, selectBody)
	if err == nil {
		t.Fatal("expected a non-object body to be refused")
	}
	if calls != 0 {
		t.Errorf("provider was called %d times, want none", calls)
	}
	if !strings.Contains(err.Error(), "JSON object") {
		t.Errorf("error %q should say the body must be a JSON object", err)
	}
}

func TestRunRefusesABodyCredentialOnAMethodWithNoBody(t *testing.T) {
	t.Setenv("TEST_SATHI_KEY", "s3cr3t")

	var sent map[string]any
	var calls int
	step := bodyAuthStep(t, http.MethodGet, `{"cropCode":"A0102"}`, &sent, &calls)

	_, err := runStep(t, step, selectBody)
	if err == nil {
		t.Fatal("expected a body credential on GET to be refused")
	}
	if calls != 0 {
		t.Errorf("provider was called %d times, want none", calls)
	}
	if !strings.Contains(err.Error(), "GET") {
		t.Errorf("error %q should name the method that carries no body", err)
	}
}

func TestBodyCredentialIsRedacted(t *testing.T) {
	t.Setenv("TEST_SATHI_KEY", "s3cr3t")

	step := newStep(t, &stubRegistry{plan: testPlan("http://upstream.invalid", http.MethodPost)},
		&stubMapper{}, func(c *Config) { c.setProviderAuth(bodyProfile()) })

	got := step.redactString(`{"apiKey":"s3cr3t"}`)
	if strings.Contains(got, "s3cr3t") || !strings.Contains(got, util.RedactedMarker) {
		t.Errorf("redactString = %q, want the credential replaced", got)
	}
}

func TestNewRefusesAHalfConfiguredBodyScheme(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		profile AuthProfile
	}{
		{"no bodyField", AuthProfile{Scheme: util.AuthSchemeBody, BodyValueEnv: "TEST_SATHI_KEY"}},
		{"no bodyValueEnv", AuthProfile{Scheme: util.AuthSchemeBody, BodyField: "apiKey"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := &Config{BindingKeys: []string{testBindingKey}, AuthByProvider: authForTestProvider(tc.profile)}
			_, _, err := New(context.Background(), nil, &stubRegistry{}, &stubMapper{}, nil, cfg)
			if err == nil {
				t.Fatal("expected a half-configured body scheme to be refused")
			}
			if !strings.Contains(err.Error(), "bodyField") {
				t.Errorf("error %q should name what is missing", err)
			}
		})
	}
}

func TestParseProviderAuthReadsTheBodyScheme(t *testing.T) {
	t.Parallel()

	profiles, err := ParseProviderAuth(map[string]string{
		"authScheme-sathi":   "body",
		"bodyField-sathi":    "apiKey",
		"bodyValueEnv-sathi": "SATHI_API_KEY",
	})
	if err != nil {
		t.Fatalf("ParseProviderAuth() error: %v", err)
	}
	got := profiles["sathi"]
	if got == nil || got.BodyField != "apiKey" || got.BodyValueEnv != "SATHI_API_KEY" {
		t.Errorf("profile = %+v, want bodyField and bodyValueEnv read", got)
	}
}
