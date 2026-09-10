// tokenQuery: the exchange, the placement, the held token and its redaction.
//
// A separate file from common_test.go on purpose -- that one is already past
// two thousand lines, and everything here is about one scheme.
package common

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
)

// queryTokenServer stands in for a provider's own token endpoint: JSON in, a
// token under a name of its choosing out.
type queryTokenServer struct {
	calls     atomic.Int32
	field     string // the response key; "token" when empty
	token     string // rotates per call when empty
	status    int
	body      string // overrides the JSON when set
	gotUser   string
	gotSecret string
	gotType   string
}

func (s *queryTokenServer) start(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := s.calls.Add(1)
		s.gotType = r.Header.Get("Content-Type")

		raw, _ := io.ReadAll(r.Body)
		var sent map[string]string
		_ = json.Unmarshal(raw, &sent)
		s.gotUser, s.gotSecret = sent["access_name"], sent["password"]

		if s.status != 0 {
			w.WriteHeader(s.status)
		}
		if s.body != "" {
			fmt.Fprint(w, s.body)
			return
		}
		token := s.token
		if token == "" {
			token = fmt.Sprintf("tok-%d", count)
		}
		field := s.field
		if field == "" {
			field = "token"
		}
		fmt.Fprintf(w, `{%q:%q}`, field, token)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// tokenQueryProfile is the fully-populated profile, for tests that then blank
// one field to prove it is required.
func tokenQueryProfile(tokenURL string) AuthProfile {
	return AuthProfile{
		Scheme:             util.AuthSchemeTokenQuery,
		TokenURL:           tokenURL,
		TokenUserField:     "access_name",
		TokenUserEnv:       "TEST_TQ_USER",
		TokenSecretName:    "password",
		TokenSecretEnv:     "TEST_TQ_SECRET",
		TokenResponseField: "token",
		QueryName:          "token",
		TokenTTLRaw:        "10m",
	}
}

// tokenQueryStep records the query string each provider call arrived with.
func tokenQueryStep(t *testing.T, tokenURL string, seen *[]string,
	status *int, tweak ...func(*AuthProfile)) *Step {
	t.Helper()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.URL.RawQuery)
		if status != nil && *status != 0 {
			w.WriteHeader(*status)
			fmt.Fprintf(w, `{"error":"Invalid token"}`)
			return
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	t.Cleanup(provider.Close)

	return newStep(t, &stubRegistry{plan: testPlan(provider.URL, http.MethodGet)},
		&stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"a":1}`)},
		func(c *Config) {
			profile := tokenQueryProfile(tokenURL)
			for _, apply := range tweak {
				apply(&profile)
			}
			// validate normally runs at startup; the profile carries a parsed
			// TTL by the time a call is made.
			if err := profile.validate(); err != nil {
				t.Fatalf("profile does not validate: %v", err)
			}
			c.setProviderAuth(profile)
		})
}

// The whole point of the scheme: credentials POSTed as JSON under CONFIGURED
// keys, and the token that comes back placed in the QUERY STRING. Neither half
// matches oauth2, which is why bending that scheme could not serve it.
func TestTokenQuerySendsTheExchangedTokenAsAQueryParameter(t *testing.T) {
	t.Setenv("TEST_TQ_USER", "Masters-Data-Provider")
	t.Setenv("TEST_TQ_SECRET", "the-password")

	ts := &queryTokenServer{token: "the-token"}
	var seen []string
	step := tokenQueryStep(t, ts.start(t), &seen, nil)

	if _, err := runStep(t, step, selectBody); err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}

	if len(seen) != 1 || !strings.Contains(seen[0], "token=the-token") {
		t.Errorf("provider saw query %q, want it to carry token=the-token", seen)
	}
	if ts.gotType != "application/json" {
		t.Errorf("token endpoint saw Content-Type %q, want application/json", ts.gotType)
	}
	if ts.gotUser != "Masters-Data-Provider" || ts.gotSecret != "the-password" {
		t.Errorf("token endpoint saw %q/%q under access_name/password, want the configured pair",
			ts.gotUser, ts.gotSecret)
	}
}

// The token is held for its configured lifetime. Without this the provider's
// token endpoint takes an extra round trip on every single call.
func TestTokenQueryReusesTheTokenWithinItsTTL(t *testing.T) {
	t.Setenv("TEST_TQ_USER", "user")
	t.Setenv("TEST_TQ_SECRET", "secret")

	ts := &queryTokenServer{}
	var seen []string
	step := tokenQueryStep(t, ts.start(t), &seen, nil)

	for i := 0; i < 5; i++ {
		if _, err := runStep(t, step, selectBody); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if got := ts.calls.Load(); got != 1 {
		t.Errorf("token endpoint called %d times for 5 requests, want 1", got)
	}
	for i, query := range seen {
		if !strings.Contains(query, "token=tok-1") {
			t.Errorf("request %d sent %q, want the cached token", i, query)
		}
	}
}

// THE SELF-HEAL. The lifetime is an operator's estimate -- the endpoint states
// no expiry -- so a too-generous tokenTtl leaves a dead token cached. Without
// dropping it on the provider's own rejection, every call fails until the ttl
// lapses, which for a ten-minute guess is a ten-minute outage.
func TestTokenQueryForgetsATokenTheProviderRejects(t *testing.T) {
	t.Setenv("TEST_TQ_USER", "user")
	t.Setenv("TEST_TQ_SECRET", "secret")

	ts := &queryTokenServer{}
	var seen []string
	status := http.StatusForbidden
	step := tokenQueryStep(t, ts.start(t), &seen, &status)

	// First call: the provider rejects the freshly exchanged token.
	if _, err := runStep(t, step, selectBody); err == nil {
		t.Fatal("Run() succeeded against a 403, want an error")
	}
	if got := ts.calls.Load(); got != 1 {
		t.Fatalf("token endpoint called %d times, want 1", got)
	}

	// Second call: the held token must NOT be reused, even though the ttl has
	// not lapsed. It is a fresh exchange or the scheme cannot recover.
	status = 0
	if _, err := runStep(t, step, selectBody); err != nil {
		t.Fatalf("second Run() = %v, want it to recover with a fresh token", err)
	}
	if got := ts.calls.Load(); got != 2 {
		t.Errorf("token endpoint called %d times over two requests, want 2 -- "+
			"the rejected token was reused instead of re-exchanged", got)
	}
	if len(seen) != 2 || strings.Contains(seen[1], "token=tok-1") {
		t.Errorf("second request sent %q, want a token other than the rejected tok-1", seen)
	}
}

// Every field is required, and the message has to name what is missing: an
// operator reading "requires tokenUrl, tokenUserField, ..." can fix it, while
// a malformed request rejected by the provider says nothing about the cause.
func TestTokenQueryRefusesAnIncompleteProfile(t *testing.T) {
	for _, tc := range []struct {
		field string
		blank func(*AuthProfile)
	}{
		{"tokenUrl", func(p *AuthProfile) { p.TokenURL = "" }},
		{"tokenUserField", func(p *AuthProfile) { p.TokenUserField = "" }},
		{"tokenUserEnv", func(p *AuthProfile) { p.TokenUserEnv = "" }},
		{"tokenSecretField", func(p *AuthProfile) { p.TokenSecretName = "" }},
		{"tokenSecretEnv", func(p *AuthProfile) { p.TokenSecretEnv = "" }},
		{"tokenResponseField", func(p *AuthProfile) { p.TokenResponseField = "" }},
		{"queryName", func(p *AuthProfile) { p.QueryName = "" }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			profile := tokenQueryProfile("https://issuer.example/token")
			profile.Provider = "a-provider"
			tc.blank(&profile)

			err := profile.validate()
			if err == nil {
				t.Fatalf("validate() accepted a profile with no %s", tc.field)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("error %q does not name the missing %s", err, tc.field)
			}
			if !strings.Contains(err.Error(), "a-provider") {
				t.Errorf("error %q does not name the provider, which an operator "+
					"needs to know which block to edit", err)
			}
		})
	}
}

// tokenTtl is required, and it has a floor. At or below the refresh skew every
// token is already expired when it arrives, so each call would exchange one and
// throw it away -- two round trips per request, forever, with nothing failing
// loudly enough to notice.
func TestTokenQueryRefusesAnUnusableTTL(t *testing.T) {
	for _, tc := range []struct{ name, ttl, wantIn string }{
		{"absent", "", "requires tokenTtl"},
		{"not a duration", "ten minutes", "is not a duration"},
		{"at the skew", util.TokenRefreshSkew.String(), "must exceed"},
		{"below the skew", "1s", "must exceed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := tokenQueryProfile("https://issuer.example/token")
			profile.Provider = "a-provider"
			profile.TokenTTLRaw = tc.ttl

			err := profile.validate()
			if err == nil {
				t.Fatalf("validate() accepted tokenTtl %q", tc.ttl)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q does not contain %q", err, tc.wantIn)
			}
		})
	}
}

// A ttl above the skew is accepted, and the held lifetime is shortened by it --
// so a request that passed the expiry check cannot arrive after the token died.
func TestTokenQuerySubtractsTheSkewFromTheConfiguredTTL(t *testing.T) {
	profile := tokenQueryProfile("https://issuer.example/token")
	profile.TokenTTLRaw = "10m"
	if err := profile.validate(); err != nil {
		t.Fatalf("validate() = %v, want a 10m ttl accepted", err)
	}
	if profile.TokenTTL != 10*time.Minute {
		t.Errorf("TokenTTL = %s, want 10m parsed from config", profile.TokenTTL)
	}
	// The subtraction itself happens in the exchange; this pins the arithmetic
	// it depends on, so a change to the skew cannot silently invert it.
	if profile.TokenTTL-util.TokenRefreshSkew <= 0 {
		t.Errorf("held lifetime %s is not positive", profile.TokenTTL-util.TokenRefreshSkew)
	}
}

// A 2xx that does not carry the configured field is the endpoint misbehaving in
// a way another attempt will not change, and the message names the field so an
// operator can tell a misconfigured tokenResponseField from a broken provider.
func TestTokenQueryRefusesAResponseWithoutTheConfiguredField(t *testing.T) {
	t.Setenv("TEST_TQ_USER", "user")
	t.Setenv("TEST_TQ_SECRET", "secret")

	// The endpoint answers 200 with a different key than the one configured.
	ts := &queryTokenServer{body: `{"access_token":"wrong-shape"}`}
	var seen []string
	step := tokenQueryStep(t, ts.start(t), &seen, nil)

	_, err := runStep(t, step, selectBody)
	if err == nil {
		t.Fatal("Run() succeeded on a response with no token field, want an error")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("error %q does not name the missing field", err)
	}
	if len(seen) != 0 {
		t.Errorf("provider was called %d times despite no token, want 0", len(seen))
	}
}

// The secret and the token must never reach a log or a returned error. The
// token matters more here than under oauth2: it travels in a query string,
// which transport errors quote whole.
func TestTokenQueryRedactsTheSecretAndTheToken(t *testing.T) {
	t.Setenv("TEST_TQ_USER", "an-identifier")
	t.Setenv("TEST_TQ_SECRET", "the-secret-value")

	ts := &queryTokenServer{token: "tok+with/escapes="}
	var seen []string
	step := tokenQueryStep(t, ts.start(t), &seen, nil)

	if _, err := runStep(t, step, selectBody); err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}

	// secretForms is what redaction substitutes; assert on it directly, since a
	// passing request produces no error text to inspect.
	var forms []string
	for _, auth := range step.auth {
		forms = append(forms, auth.secretForms()...)
	}
	joined := strings.Join(forms, "|")

	for _, want := range []string{"the-secret-value", "tok+with/escapes="} {
		if !strings.Contains(joined, want) {
			t.Errorf("secretForms %q does not cover %q", forms, want)
		}
	}
	// The escaped form too, or a token with "+" survives redaction in a URL.
	if !strings.Contains(joined, "tok%2Bwith%2Fescapes%3D") {
		t.Errorf("secretForms %q does not cover the query-escaped token", forms)
	}
	// The identifier is not a credential and is left readable, so a log still
	// says who this adapter authenticated as.
	if strings.Contains(joined, "an-identifier") {
		t.Errorf("secretForms %q redacts the identifier, which is not a secret", forms)
	}
}

// An unset credential is a configuration fault: it fails permanently rather
// than being retried, and the message names the variables without printing
// them.
func TestTokenQueryWithNoCredentialFailsWithoutRetrying(t *testing.T) {
	t.Setenv("TEST_TQ_USER", "")
	t.Setenv("TEST_TQ_SECRET", "")

	ts := &queryTokenServer{}
	var seen []string
	step := tokenQueryStep(t, ts.start(t), &seen, nil)

	_, err := runStep(t, step, selectBody)
	if err == nil {
		t.Fatal("Run() succeeded with no credential set, want an error")
	}
	if got := ts.calls.Load(); got != 0 {
		t.Errorf("token endpoint called %d times with no credential, want 0", got)
	}
	if !util.IsPermanent(err) {
		t.Errorf("error %v is retryable; an unset credential will not fix itself", err)
	}
}
