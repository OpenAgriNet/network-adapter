// tokenHeader: tokenQuery's exchange, the bare token sent in a configured header.
package common

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
)

// assertStatus fails unless err is a CodedErr carrying status.
func assertStatus(t *testing.T, err error, status int) {
	t.Helper()
	var coded *model.CodedErr
	if !errors.As(err, &coded) || coded.HTTPStatus() != status {
		t.Fatalf("error = %v, want a coded %d", err, status)
	}
}

// tokenHeaderProfile is tokenQuery's profile under the header scheme. queryName
// is cleared to prove it is not needed.
func tokenHeaderProfile(tokenURL string) AuthProfile {
	profile := tokenQueryProfile(tokenURL)
	profile.Scheme = util.AuthSchemeTokenHeader
	profile.QueryName = ""
	profile.HeaderName = "Authorization"
	return profile
}

// tokenHeaderStep records the Authorization header and query each provider call
// arrived with. status, when non-zero, is what the provider answers.
func tokenHeaderStep(t *testing.T, tokenURL string, headers, queries *[]string, status *int) *Step {
	t.Helper()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*headers = append(*headers, r.Header.Get("Authorization"))
		*queries = append(*queries, r.URL.RawQuery)
		if status != nil && *status != 0 {
			w.WriteHeader(*status)
			return
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	t.Cleanup(provider.Close)

	return newStep(t, &stubRegistry{plan: testPlan(provider.URL, http.MethodPost)},
		&stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"a":1}`)},
		func(c *Config) {
			profile := tokenHeaderProfile(tokenURL)
			if err := profile.validate(); err != nil {
				t.Fatalf("profile does not validate: %v", err)
			}
			c.setProviderAuth(profile)
		})
}

func TestTokenHeader_ValidLogin_SendsBareTokenInHeaderNotQuery(t *testing.T) {
	t.Setenv("TEST_TQ_USER", "pmfby-user")
	t.Setenv("TEST_TQ_SECRET", "pmfby-password")

	ts := &queryTokenServer{token: "the-token"}
	var headers, queries []string
	step := tokenHeaderStep(t, ts.start(t), &headers, &queries, nil)

	if _, err := runStep(t, step, selectBody); err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}
	if len(headers) != 1 || headers[0] != "the-token" {
		t.Errorf("provider saw Authorization %q, want the bare token, no Bearer prefix", headers)
	}
	if strings.Contains(queries[0], "the-token") {
		t.Errorf("provider saw query %q; the token belongs in the header only", queries[0])
	}
	if ts.gotUser != "pmfby-user" || ts.gotSecret != "pmfby-password" || ts.gotType != "application/json" {
		t.Errorf("login saw %q/%q as %q, want the configured pair as JSON", ts.gotUser, ts.gotSecret, ts.gotType)
	}
}

func TestTokenHeader_RepeatedCalls_LogsInOnce(t *testing.T) {
	t.Setenv("TEST_TQ_USER", "user")
	t.Setenv("TEST_TQ_SECRET", "secret")

	ts := &queryTokenServer{}
	var headers, queries []string
	step := tokenHeaderStep(t, ts.start(t), &headers, &queries, nil)

	for i := 0; i < 3; i++ {
		if _, err := runStep(t, step, selectBody); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if got := ts.calls.Load(); got != 1 {
		t.Errorf("login called %d times for 3 requests, want 1", got)
	}
}

func TestTokenHeader_ProviderRejectsToken_LogsInAgainNextCall(t *testing.T) {
	t.Setenv("TEST_TQ_USER", "user")
	t.Setenv("TEST_TQ_SECRET", "secret")

	ts := &queryTokenServer{}
	var headers, queries []string
	status := http.StatusUnauthorized
	step := tokenHeaderStep(t, ts.start(t), &headers, &queries, &status)

	if _, err := runStep(t, step, selectBody); err == nil {
		t.Fatal("Run() succeeded against a 401, want an error")
	}
	status = 0
	if _, err := runStep(t, step, selectBody); err != nil {
		t.Fatalf("second Run() = %v, want it to recover with a fresh token", err)
	}
	if got := ts.calls.Load(); got != 2 || headers[1] != "tok-2" {
		t.Errorf("login called %d times, second call sent %q; want 2 and tok-2", got, headers[1])
	}
}

func TestTokenHeader_LoginFails_Returns502WithoutCallingProvider(t *testing.T) {
	t.Setenv("TEST_TQ_USER", "user")
	t.Setenv("TEST_TQ_SECRET", "wrong")

	ts := &queryTokenServer{status: http.StatusUnauthorized, body: `{"message":"invalid credentials"}`}
	var headers, queries []string
	step := tokenHeaderStep(t, ts.start(t), &headers, &queries, nil)

	_, err := runStep(t, step, selectBody)
	assertStatus(t, err, http.StatusBadGateway)
	if len(headers) != 0 {
		t.Errorf("provider was called %d times without a token, want 0", len(headers))
	}
}

func TestTokenHeader_LoginResponseWithoutToken_Returns502(t *testing.T) {
	t.Setenv("TEST_TQ_USER", "user")
	t.Setenv("TEST_TQ_SECRET", "secret")

	ts := &queryTokenServer{field: "accessToken"}
	var headers, queries []string
	step := tokenHeaderStep(t, ts.start(t), &headers, &queries, nil)

	_, err := runStep(t, step, selectBody)
	assertStatus(t, err, http.StatusBadGateway)
}

func TestTokenHeader_CredentialUnset_FailsPermanentlyWithoutLogin(t *testing.T) {
	t.Setenv("TEST_TQ_USER", "")
	t.Setenv("TEST_TQ_SECRET", "")

	ts := &queryTokenServer{}
	var headers, queries []string
	step := tokenHeaderStep(t, ts.start(t), &headers, &queries, nil)

	_, err := runStep(t, step, selectBody)
	if err == nil || ts.calls.Load() != 0 || len(headers) != 0 {
		t.Fatalf("err %v, %d logins, %d provider calls; want an error and no calls", err, ts.calls.Load(), len(headers))
	}
	if strings.Contains(err.Error(), "TEST_TQ_USER") {
		t.Errorf("error %q names the environment variable", err)
	}
	if !strings.Contains(err.Error(), util.AuthSchemeTokenHeader) {
		t.Errorf("error %q should name the scheme", err)
	}
}

func TestTokenHeader_AfterCall_RedactsSecretAndToken(t *testing.T) {
	t.Setenv("TEST_TQ_USER", "an-identifier")
	t.Setenv("TEST_TQ_SECRET", "the-secret-value")

	ts := &queryTokenServer{token: "the-token-value"}
	var headers, queries []string
	step := tokenHeaderStep(t, ts.start(t), &headers, &queries, nil)
	if _, err := runStep(t, step, selectBody); err != nil {
		t.Fatal(err)
	}

	redacted := step.redactString("secret the-secret-value token the-token-value user an-identifier")
	if strings.Contains(redacted, "the-secret-value") || strings.Contains(redacted, "the-token-value") {
		t.Errorf("redacted %q still carries a credential", redacted)
	}
	if !strings.Contains(redacted, "an-identifier") {
		t.Errorf("redacted %q hides the identifier, which is not a secret", redacted)
	}
}

func TestTokenHeader_IncompleteProfile_RefusedAtStartup(t *testing.T) {
	for name, blank := range map[string]func(*AuthProfile){
		"tokenUrl":                func(p *AuthProfile) { p.TokenURL = "" },
		"tokenUserField":          func(p *AuthProfile) { p.TokenUserField = "" },
		"tokenUserEnv":            func(p *AuthProfile) { p.TokenUserEnv = "" },
		"tokenSecretField":        func(p *AuthProfile) { p.TokenSecretName = "" },
		"tokenSecretEnv":          func(p *AuthProfile) { p.TokenSecretEnv = "" },
		"tokenResponseField":      func(p *AuthProfile) { p.TokenResponseField = "" },
		"tokenTtl":                func(p *AuthProfile) { p.TokenTTLRaw = "" },
		"headerName":              func(p *AuthProfile) { p.HeaderName = "" },
		"tokenTtl not a duration": func(p *AuthProfile) { p.TokenTTLRaw = "ten minutes" },
		"tokenTtl within skew":    func(p *AuthProfile) { p.TokenTTLRaw = "30s" },
	} {
		t.Run(name, func(t *testing.T) {
			profile := tokenHeaderProfile("https://login.example")
			profile.Provider = "pmfby"
			blank(&profile)
			err := profile.validate()
			if err == nil {
				t.Fatal("validate() accepted an incomplete tokenHeader profile")
			}
			if !strings.Contains(err.Error(), "pmfby") {
				t.Errorf("error %q should name the provider", err)
			}
		})
	}
}

func TestTokenQuery_WithoutQueryName_StillRefused(t *testing.T) {
	profile := tokenQueryProfile("https://login.example")
	profile.QueryName = ""
	if err := profile.validate(); err == nil || !strings.Contains(err.Error(), "queryName") {
		t.Errorf("validate() = %v, want tokenQuery refused without queryName", err)
	}
}

func TestParseProviderAuth_TokenHeaderBlock_BuildsProfile(t *testing.T) {
	profiles, err := ParseProviderAuth(map[string]string{
		"authScheme-pmfby":         "tokenHeader",
		"tokenUrl-pmfby":           "https://login.example",
		"tokenUserField-pmfby":     "userName",
		"tokenUserEnv-pmfby":       "PMFBY_USER",
		"tokenSecretField-pmfby":   "password",
		"tokenSecretEnv-pmfby":     "PMFBY_PASSWORD",
		"tokenResponseField-pmfby": "token",
		"tokenTtl-pmfby":           "10m",
		"headerName-pmfby":         "Authorization",
	})
	if err != nil {
		t.Fatal(err)
	}
	profile := profiles["pmfby"]
	if profile == nil || profile.Scheme != util.AuthSchemeTokenHeader {
		t.Fatalf("profiles = %+v, want a tokenHeader profile for pmfby", profiles)
	}
	if err := profile.validate(); err != nil {
		t.Errorf("validate() = %v, want the parsed profile accepted", err)
	}
}
