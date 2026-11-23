package common

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
)

const graphqlLogin = `query Query($refreshToken: String!) { generateAccessToken(refreshToken: $refreshToken) }`

func graphqlTokenServer(t *testing.T, loginBody *map[string]any) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, loginBody); err != nil {
			t.Errorf("login body is not JSON: %v", err)
		}
		fmt.Fprint(w, `{"data":{"generateAccessToken":{"token":"acc-1","refreshToken":"rotated"}}}`)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func graphqlProfile(tokenURL string) AuthProfile {
	return AuthProfile{
		Scheme:             util.AuthSchemeTokenHeader,
		TokenURL:           tokenURL,
		TokenRequestBody:   `{"query":"` + graphqlLogin + `"}`,
		TokenSecretName:    "variables.refreshToken",
		TokenSecretEnv:     "TEST_SHC_REFRESH",
		TokenResponseField: "data.generateAccessToken.token",
		TokenTTLRaw:        "144h",
		HeaderName:         "Authorization",
		TokenHeaderPrefix:  "Bearer",
	}
}

func graphqlStep(t *testing.T, profile AuthProfile, sentAuth *string) *Step {
	t.Helper()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*sentAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	t.Cleanup(provider.Close)
	return newStep(t, &stubRegistry{plan: testPlan(provider.URL, http.MethodPost)},
		&stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"a":1}`)},
		func(c *Config) {
			if err := profile.validate(); err != nil {
				t.Fatalf("profile does not validate: %v", err)
			}
			c.setProviderAuth(profile)
		})
}

func TestTokenHeaderSendsAGraphQLLoginBodyWithANestedSecret(t *testing.T) {
	t.Setenv("TEST_SHC_REFRESH", "r3fresh")

	var login map[string]any
	var sentAuth string
	step := graphqlStep(t, graphqlProfile(graphqlTokenServer(t, &login)), &sentAuth)

	if _, err := runStep(t, step, selectBody); err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}
	want := map[string]any{"query": graphqlLogin, "variables": map[string]any{"refreshToken": "r3fresh"}}
	if !reflect.DeepEqual(login, want) {
		t.Errorf("login body = %v, want %v", login, want)
	}
	if sentAuth != "Bearer acc-1" {
		t.Errorf("Authorization = %q, want the token behind the configured prefix", sentAuth)
	}
}

func TestTokenHeaderWithoutAPrefixSendsTheTokenBare(t *testing.T) {
	t.Setenv("TEST_SHC_REFRESH", "r3fresh")

	var login map[string]any
	var sentAuth string
	profile := graphqlProfile(graphqlTokenServer(t, &login))
	profile.TokenHeaderPrefix = ""
	step := graphqlStep(t, profile, &sentAuth)

	if _, err := runStep(t, step, selectBody); err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}
	if sentAuth != "acc-1" {
		t.Errorf("Authorization = %q, want the bare token", sentAuth)
	}
}

func TestTokenSchemeRefusesABodyThatIsNotAJSONObject(t *testing.T) {
	t.Parallel()

	for _, base := range []string{`not json`, `["query"]`, `"query"`} {
		profile := graphqlProfile("http://issuer.invalid")
		profile.Provider = testProvider
		profile.TokenRequestBody = base
		err := profile.validate()
		if err == nil || !strings.Contains(err.Error(), "tokenRequestBody") {
			t.Errorf("tokenRequestBody %q: err = %v, want a refusal naming tokenRequestBody", base, err)
		}
	}
}

func TestTokenSchemeRefusesHalfAUserSetting(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		tweak func(*AuthProfile)
	}{
		{"field without env", func(p *AuthProfile) { p.TokenUserField = "access_name" }},
		{"env without field", func(p *AuthProfile) { p.TokenUserEnv = "TEST_USER" }},
	} {
		profile := graphqlProfile("http://issuer.invalid")
		profile.Provider = testProvider
		tc.tweak(&profile)
		if err := profile.validate(); err == nil || !strings.Contains(err.Error(), "tokenUserField") {
			t.Errorf("%s: err = %v, want a refusal naming tokenUserField", tc.name, err)
		}
	}
}

func TestPlainTokenLoginStillRequiresAUser(t *testing.T) {
	t.Parallel()

	profile := graphqlProfile("http://issuer.invalid")
	profile.Provider = testProvider
	profile.TokenRequestBody = ""
	if err := profile.validate(); err == nil || !strings.Contains(err.Error(), "tokenUserField") {
		t.Errorf("err = %v, want a login with no request body and no user refused", err)
	}
}

func TestTokenSchemeRefusesASecretPathThatCollidesWithTheBody(t *testing.T) {
	t.Setenv("TEST_SHC_REFRESH", "r3fresh")

	var login map[string]any
	var sentAuth string
	profile := graphqlProfile(graphqlTokenServer(t, &login))
	profile.TokenSecretName = "query.refreshToken"
	step := graphqlStep(t, profile, &sentAuth)

	if _, err := runStep(t, step, selectBody); err == nil {
		t.Fatal("expected a secret path through a non-object base field to be refused")
	}
	if sentAuth != "" {
		t.Error("the provider was called without a token")
	}
}

func TestParseProviderAuthReadsTheGraphQLTokenSettings(t *testing.T) {
	t.Parallel()

	profiles, err := ParseProviderAuth(map[string]string{
		"authScheme-shc":        "tokenHeader",
		"tokenRequestBody-shc":  `{"query":"q"}`,
		"tokenHeaderPrefix-shc": "Bearer",
	})
	if err != nil {
		t.Fatalf("ParseProviderAuth() error: %v", err)
	}
	got := profiles["shc"]
	if got.TokenRequestBody != `{"query":"q"}` || got.TokenHeaderPrefix != "Bearer" {
		t.Errorf("profile = %+v, want tokenRequestBody and tokenHeaderPrefix read", got)
	}
}

func TestGraphQLRefreshTokenIsRedacted(t *testing.T) {
	t.Setenv("TEST_SHC_REFRESH", "r3fresh")

	step := newStep(t, &stubRegistry{plan: testPlan("http://upstream.invalid", http.MethodPost)},
		&stubMapper{}, func(c *Config) {
			profile := graphqlProfile("http://issuer.invalid")
			if err := profile.validate(); err != nil {
				t.Fatalf("profile does not validate: %v", err)
			}
			c.setProviderAuth(profile)
		})
	if got := step.redactString(`{"refreshToken":"r3fresh"}`); strings.Contains(got, "r3fresh") {
		t.Errorf("redactString = %q, want the refresh token replaced", got)
	}
}
