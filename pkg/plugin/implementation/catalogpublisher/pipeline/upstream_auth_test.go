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
	"io"
	"net/http"
	"net/http/httptest"
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
