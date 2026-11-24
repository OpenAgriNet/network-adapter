// tokenEncoding: the same exchange written as a form, with the constant fields
// an issuer wants beside the credential. KCC's login is the reason both exist.
package common

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
)

// formTokenServer reads the login as a form, the way KCC's does, and answers
// with the token nested under data -- also the way KCC's does.
type formTokenServer struct {
	calls   atomic.Int32
	gotType string
	gotBody string
	got     url.Values
}

func (s *formTokenServer) start(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		s.gotType = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		s.gotBody = string(raw)
		s.got, _ = url.ParseQuery(s.gotBody)
		fmt.Fprint(w, `{"data":{"token":"kcc-token","sessionTTL":3600}}`)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// kccTokenProfile is the login KCC actually describes: three form fields, the
// token two levels down, a lifetime this side states rather than reads.
func kccTokenProfile(tokenURL string) AuthProfile {
	return AuthProfile{
		Provider:            "kcc",
		Scheme:              util.AuthSchemeTokenHeader,
		TokenURL:            tokenURL,
		TokenEncoding:       util.TokenEncodingForm,
		TokenExtraFieldsRaw: "entityType=KRISHIKA",
		TokenUserField:      "userID",
		TokenUserEnv:        "TEST_KCC_USER",
		TokenSecretName:     "password",
		TokenSecretEnv:      "TEST_KCC_SECRET",
		TokenResponseField:  "data.token",
		HeaderName:          "token",
		TokenTTLRaw:         "1h",
	}
}

// formTokenStep records the header each provider call arrived with.
func formTokenStep(t *testing.T, tokenURL string, headers *[]string,
	tweak ...func(*AuthProfile)) *Step {
	t.Helper()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*headers = append(*headers, r.Header.Get("token"))
		fmt.Fprint(w, `{"ok":true}`)
	}))
	t.Cleanup(provider.Close)

	return newStep(t, &stubRegistry{plan: testPlan(provider.URL, http.MethodPost)},
		&stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"a":1}`)},
		func(c *Config) {
			profile := kccTokenProfile(tokenURL)
			for _, apply := range tweak {
				apply(&profile)
			}
			if err := profile.validate(); err != nil {
				t.Fatalf("profile does not validate: %v", err)
			}
			c.setProviderAuth(profile)
		})
}

// The whole point: KCC's login is form-encoded, carries a constant entityType
// beside the credential, and hides the token under data. A JSON body here is
// answered with a 415, so this is the difference between working and not.
func TestTokenFormSendsAFormWithTheExtraFieldBesideTheCredential(t *testing.T) {
	t.Setenv("TEST_KCC_USER", "vistaar")
	t.Setenv("TEST_KCC_SECRET", "the-password")

	ts := &formTokenServer{}
	var headers []string
	step := formTokenStep(t, ts.start(t), &headers)

	if _, err := runStep(t, step, selectBody); err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}
	if ts.gotType != "application/x-www-form-urlencoded" {
		t.Errorf("token endpoint saw Content-Type %q, want the form type", ts.gotType)
	}
	if strings.HasPrefix(strings.TrimSpace(ts.gotBody), "{") {
		t.Errorf("token endpoint saw %q, want a form body, not JSON", ts.gotBody)
	}
	for field, want := range map[string]string{
		"userID": "vistaar", "password": "the-password", "entityType": "KRISHIKA",
	} {
		if got := ts.got.Get(field); got != want {
			t.Errorf("token endpoint saw %s=%q, want %q", field, got, want)
		}
	}
	if len(headers) != 1 || headers[0] != "kcc-token" {
		t.Errorf("provider saw token header %q, want the token read from data.token", headers)
	}
}

// The extra fields are not a form-only feature: an issuer wanting a constant
// beside a JSON credential gets the same thing, so the two settings compose.
func TestTokenExtraFieldsAlsoTravelInAJSONLogin(t *testing.T) {
	t.Setenv("TEST_KCC_USER", "vistaar")
	t.Setenv("TEST_KCC_SECRET", "the-password")

	var gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		fmt.Fprint(w, `{"data":{"token":"kcc-token"}}`)
	}))
	t.Cleanup(server.Close)

	var headers []string
	step := formTokenStep(t, server.URL, &headers, func(p *AuthProfile) {
		p.TokenEncoding = ""
	})
	if _, err := runStep(t, step, selectBody); err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}
	if !strings.Contains(gotBody, `"entityType":"KRISHIKA"`) {
		t.Errorf("token endpoint saw %q, want the extra field in the JSON body", gotBody)
	}
}

// Encoding is a closed set. A typo that silently fell back to JSON would be
// found by a provider's 415, in production, instead of here.
func TestTokenEncodingIsAClosedSet(t *testing.T) {
	profile := kccTokenProfile("http://example.invalid/token")
	profile.TokenEncoding = "urlencoded"
	err := profile.validate()
	if err == nil || !strings.Contains(err.Error(), "tokenEncoding") {
		t.Fatalf("validate() = %v, want a complaint about tokenEncoding", err)
	}
}

// Every way of writing the pairs wrongly, refused at startup rather than
// turning into a field the issuer does not recognise.
func TestTokenExtraFieldsAreValidated(t *testing.T) {
	cases := map[string]struct {
		raw  string
		want string // "" means it must be accepted
	}{
		"one pair":            {"entityType=KRISHIKA", ""},
		"several pairs":       {"entityType=KRISHIKA,channel=WEB", ""},
		"spaces around pairs": {" entityType = KRISHIKA , channel = WEB ", ""},
		"empty is allowed":    {"", ""},
		"no equals":           {"entityType", "name=value"},
		"no name":             {"=KRISHIKA", "name=value"},
		"no value":            {"entityType=", "name=value"},
		"named twice":         {"entityType=A,entityType=B", "twice"},
		"shadows the user":    {"userID=someone", "tokenUserField"},
		"shadows the secret":  {"password=guess", "tokenUserField"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			profile := kccTokenProfile("http://example.invalid/token")
			profile.TokenExtraFieldsRaw = tc.raw
			err := profile.validate()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("validate(%q) = %v, want it accepted", tc.raw, err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("validate(%q) = %v, want a complaint mentioning %q", tc.raw, err, tc.want)
			}
		})
	}
}

// The parsed pairs are what reaches the request, trimmed, in any order.
func TestTokenExtraFieldsParseIntoTheSentPairs(t *testing.T) {
	profile := kccTokenProfile("http://example.invalid/token")
	profile.TokenExtraFieldsRaw = " entityType = KRISHIKA , channel = WEB "
	if err := profile.validate(); err != nil {
		t.Fatalf("validate() returned an unexpected error: %v", err)
	}
	want := map[string]string{"entityType": "KRISHIKA", "channel": "WEB"}
	for name, value := range want {
		if profile.TokenExtraFields[name] != value {
			t.Errorf("TokenExtraFields[%q] = %q, want %q",
				name, profile.TokenExtraFields[name], value)
		}
	}
	if len(profile.TokenExtraFields) != len(want) {
		t.Errorf("TokenExtraFields = %v, want exactly %v", profile.TokenExtraFields, want)
	}
}

// tokenRequestBody is the one place the two encodings differ; pinned directly
// so a change to it cannot pass by only satisfying the form server above.
func TestTokenRequestBodyWritesBothEncodings(t *testing.T) {
	profile := AuthProfile{
		TokenUserField:  "userID",
		TokenSecretName: "password",
		TokenEncoding:   util.TokenEncodingForm,
	}

	form, formType, err := tokenRequestBody(profile, "a b", "p&w")
	if err != nil {
		t.Fatalf("tokenRequestBody(form) returned an unexpected error: %v", err)
	}
	if formType != "application/x-www-form-urlencoded" {
		t.Errorf("form Content-Type = %q", formType)
	}
	got, err := url.ParseQuery(string(form))
	if err != nil {
		t.Fatalf("form body %q does not parse: %v", form, err)
	}
	if got.Get("userID") != "a b" || got.Get("password") != "p&w" {
		t.Errorf("form body %q loses a value to escaping", form)
	}

	profile.TokenEncoding = ""
	js, jsType, err := tokenRequestBody(profile, "a b", "p&w")
	if err != nil {
		t.Fatalf("tokenRequestBody(json) returned an unexpected error: %v", err)
	}
	if jsType != "application/json" || !strings.Contains(string(js), `"userID":"a b"`) {
		t.Errorf("json body = %q (%s), want the fields as JSON", js, jsType)
	}
}
