package pipeline

// auth_test.go covers the token exchange, which is now entirely declared by
// the pipeline file rather than known to this package.
//
// Two things are under test: that the file's method, path, body keys and
// token location are really the ones used, and that nothing about a
// credential can reach an error. The second matters because this upstream
// echoes the request back in rejection bodies.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func authSpec() Auth {
	return Auth{
		Kind: "tokenExchange",
		Request: AuthRequest{
			Method: http.MethodPost,
			Path:   "/v9/mint-a-token",
			Body: map[string]string{
				"access_name": "${inputs.tokenUser}",
				"password":    "${inputs.tokenSecret}",
			},
		},
		Token: AuthTokenSpec{At: "$.token", CarriedAs: "query", Name: "token"},
	}
}

func authInputs() *runContext {
	return newRunContext(map[string]string{
		"tokenUser":   "user1",
		"tokenSecret": "secret1",
	}, "")
}

// The file's path and body keys must be the ones actually sent -- that is the
// whole point of declaring them.
func TestTokenUsesTheDeclaredPathAndBodyKeys(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"token":"tok-abc"}`))
	}))
	defer upstream.Close()

	client := NewClient(upstream.URL)
	client.http = upstream.Client()

	token, err := client.Token(context.Background(), authSpec(), authInputs())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token != "tok-abc" {
		t.Errorf("token = %q", token)
	}
	if gotPath != "/v9/mint-a-token" {
		t.Errorf("path = %q, want the file's own path", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q", gotMethod)
	}
	if gotBody["access_name"] != "user1" || gotBody["password"] != "secret1" {
		t.Errorf("body = %v, want the file's key names carrying the resolved inputs", gotBody)
	}
}

// A different upstream spells all of it differently. Nothing in Go should
// need to change for that.
func TestTokenFollowsADifferentUpstreamsSpelling(t *testing.T) {
	var gotPath string
	var gotBody map[string]string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"data":{"access_token":"tok-nested"}}`))
	}))
	defer upstream.Close()

	auth := Auth{
		Kind:    "tokenExchange",
		Request: AuthRequest{Method: http.MethodPost, Path: "/oauth/token", Body: map[string]string{"client_id": "${inputs.tokenUser}"}},
		Token:   AuthTokenSpec{At: "$.data.access_token"},
	}

	client := NewClient(upstream.URL)
	client.http = upstream.Client()

	token, err := client.Token(context.Background(), auth, authInputs())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token != "tok-nested" {
		t.Errorf("token = %q, want the value at the declared nested path", token)
	}
	if gotPath != "/oauth/token" || gotBody["client_id"] != "user1" {
		t.Errorf("path = %q body = %v", gotPath, gotBody)
	}
}

func TestTokenRefusesAnUnusableAuthBlock(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"token":"tok"}`))
	}))
	defer upstream.Close()

	tests := map[string]Auth{
		"unsupported kind":       {Kind: "mutualTLS", Request: AuthRequest{Path: "/t"}, Token: AuthTokenSpec{At: "$.token"}},
		"no path":                {Kind: "tokenExchange", Token: AuthTokenSpec{At: "$.token"}},
		"no token location":      {Kind: "tokenExchange", Request: AuthRequest{Path: "/t"}},
		"token location no root": {Kind: "tokenExchange", Request: AuthRequest{Path: "/t"}, Token: AuthTokenSpec{At: "token"}},
		// "body" is not a valid token placement; only "query" and "header" are.
		"unsupported placement": {Kind: "tokenExchange", Request: AuthRequest{Path: "/t"},
			Token: AuthTokenSpec{At: "$.token", CarriedAs: "body"}},
	}

	for name, auth := range tests {
		t.Run(name, func(t *testing.T) {
			client := NewClient(upstream.URL)
			client.http = upstream.Client()
			if _, err := client.Token(context.Background(), auth, authInputs()); err == nil {
				t.Error("an unusable auth block was accepted")
			}
		})
	}
}

// A credential that resolved to nothing must be refused here, not sent as an
// empty string and rejected far away as "bad credentials".
func TestTokenRefusesAnEmptyCredential(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"token":"tok"}`))
	}))
	defer upstream.Close()

	client := NewClient(upstream.URL)
	client.http = upstream.Client()

	rc := newRunContext(map[string]string{"tokenUser": "", "tokenSecret": "s"}, "")
	_, err := client.Token(context.Background(), authSpec(), rc)
	if err == nil {
		t.Fatal("an empty credential was sent")
	}
	if !strings.Contains(err.Error(), "access_name") {
		t.Errorf("error %q does not name the field that was empty", err)
	}
}

// The credential must never appear in an error, whatever the upstream does --
// and this upstream echoes the request back in its rejection body.
func TestTokenNeverLeaksTheCredential(t *testing.T) {
	const secret = "pw-SECRET-do-not-leak-4b2e"

	cases := map[string]http.HandlerFunc{
		"rejection echoing the body": func(w http.ResponseWriter, r *http.Request) {
			body, _ := json.Marshal(map[string]any{"error": "bad credentials", "received": readAll(r)})
			http.Error(w, string(body), http.StatusUnauthorized)
		},
		"success carrying no token": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"received":"` + readAll(r) + `"}`))
		},
		"not JSON at all": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("upstream is down; you sent " + readAll(r)))
		},
	}

	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			upstream := httptest.NewServer(handler)
			defer upstream.Close()

			client := NewClient(upstream.URL)
			client.http = upstream.Client()

			rc := newRunContext(map[string]string{"tokenUser": "u", "tokenSecret": secret}, "")
			_, err := client.Token(context.Background(), authSpec(), rc)
			if err == nil {
				t.Fatal("want an error")
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("the credential leaked into an error: %v", err)
			}
		})
	}
}

// An unreachable endpoint must not wrap Go's transport error: it quotes the
// whole URL, and a token endpoint's URL is where a credential could sit.
func TestTokenDoesNotQuoteTheURLWhenUnreachable(t *testing.T) {
	client := NewClient("http://127.0.0.1:1")
	_, err := client.Token(context.Background(), authSpec(), authInputs())
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("the error quotes the endpoint URL: %v", err)
	}
}

func readAll(r *http.Request) string {
	body, _ := io.ReadAll(r.Body)
	return string(body)
}

// The token exchange must refuse to send credentials in cleartext.
//
// The pipeline used to default baseUrl to a plain-HTTP address at a bare IP,
// so a deployment that forgot to set the env var POSTed its credentials, and
// then carried its token in every query string, unencrypted. Removing the
// default stops that one file; this stops any file.
func TestTokenRefusesCleartextUpstream(t *testing.T) {
	client := NewClient("http://an-upstream.test")
	_, err := client.Token(context.Background(), authSpec(), authInputs())
	if err == nil {
		t.Fatal("credentials were sent to a plain-HTTP upstream")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("error %q does not say what is required", err)
	}
}

// Loopback is the exception: a developer's fake upstream and every test in
// this package run on http://127.0.0.1, and refusing those would make the
// rule unusable rather than safe.
func TestTokenAllowsCleartextOnLoopback(t *testing.T) {
	for _, host := range []string{"http://127.0.0.1:8080", "http://localhost:8080", "http://[::1]:8080"} {
		if err := checkUpstreamScheme(host, false); err != nil {
			t.Errorf("loopback %s was refused: %v", host, err)
		}
	}
	if err := checkUpstreamScheme("https://real.test", false); err != nil {
		t.Errorf("https was refused: %v", err)
	}
}

// An upstream with no TLS at all is a real case -- Agmarknet answers on
// neither 443 nor its own port over https -- so the rule has an escape hatch.
// It must be an explicit, declared one, never an accident.
func TestCleartextIsAllowedOnlyWhenDeclared(t *testing.T) {
	const plain = "http://an-upstream.test"

	if err := checkUpstreamScheme(plain, false); err == nil {
		t.Error("cleartext was allowed without the file asking for it")
	} else if !strings.Contains(err.Error(), "allowCleartext") {
		t.Errorf("error %q does not say how to declare the exception", err)
	}

	if err := checkUpstreamScheme(plain, true); err != nil {
		t.Errorf("a declared cleartext upstream was still refused: %v", err)
	}

	// The escape hatch must not become a way to skip the check entirely: a
	// missing scheme is a broken address, not a cleartext one.
	if err := checkUpstreamScheme("an-upstream.test", true); err == nil {
		t.Error("an address with no scheme was accepted because cleartext was allowed")
	}
}

// A redirect must not carry the credentials to a host the file never named.
//
// The token exchange POSTs the credentials themselves, and a 307 preserves
// both method and body -- so an upstream (or anything that can answer as one)
// replying "307 Location: https://elsewhere/" would have Go re-send the
// username and password there. The data calls are the same shape with the
// token in the query.
func TestARedirectDoesNotCarryCredentialsToAnotherHost(t *testing.T) {
	var reachedElsewhere bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reachedElsewhere = true
		_, _ = w.Write([]byte(`{"token":"tok-from-the-wrong-host"}`))
	}))
	defer elsewhere.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer upstream.Close()

	client := NewClient(upstream.URL)
	client.http = upstream.Client()

	_, err := client.Token(context.Background(), authSpec(), authInputs())
	if err == nil {
		t.Fatal("a cross-host redirect was followed with the credentials attached")
	}
	if reachedElsewhere {
		t.Error("the credentials were re-sent to the redirect target")
	}
	// The reason has to survive: reported as "could not be reached", an
	// operator chases a network fault that is not there.
	if !errors.Is(err, ErrRedirectRefused) {
		t.Errorf("the refusal reason was lost; error was %v", err)
	}
	if strings.Contains(err.Error(), "could not be reached") {
		t.Errorf("a refused redirect was reported as unreachable: %v", err)
	}
	for _, secret := range []string{"user1", "secret1"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the error quotes a credential: %v", err)
		}
	}
}

// A redirect that stays on the declared host is ordinary and must still work.
func TestARedirectOnTheSameHostIsFollowed(t *testing.T) {
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/moved" {
			http.Redirect(w, r, upstream.URL+"/moved", http.StatusTemporaryRedirect)
			return
		}
		_, _ = w.Write([]byte(`{"token":"tok-abc"}`))
	}))
	defer upstream.Close()

	client := NewClient(upstream.URL)
	client.http = upstream.Client()

	token, err := client.Token(context.Background(), authSpec(), authInputs())
	if err != nil {
		t.Fatalf("a same-host redirect was refused: %v", err)
	}
	if token != "tok-abc" {
		t.Errorf("token = %q, want tok-abc", token)
	}
}

// A POST must carry the token under the name the FILE declares.
//
// `upstream.auth.token.name` is the whole reason that key exists: a second
// upstream spelling it "api_key" or "apikey" would silently authenticate as
// nobody if the engine kept its own spelling.
func TestPostCarriesTheTokenUnderTheDeclaredName(t *testing.T) {
	var gotQuery url.Values
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v9/mint-a-token" {
			_, _ = w.Write([]byte(`{"token":"tok-abc"}`))
			return
		}
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte(`{"records":[]}`))
	}))
	defer upstream.Close()

	auth := authSpec()
	auth.Token.Name = "api_key"

	client := NewClient(upstream.URL)
	client.http = upstream.Client()
	token, err := client.Token(context.Background(), auth, authInputs())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}

	if _, err := client.fetchPost(context.Background(), "/data", []byte(`{}`), token); err != nil {
		t.Fatalf("fetchPost: %v", err)
	}
	if got := gotQuery.Get("api_key"); got != "tok-abc" {
		t.Errorf("the token was not sent as api_key; query was %v", gotQuery)
	}
	if gotQuery.Has("token") {
		t.Error("the token was also sent under the engine's own spelling")
	}
}
