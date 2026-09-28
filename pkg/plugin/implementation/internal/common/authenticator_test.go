// authenticator_test.go proves the credential machinery works standalone --
// without a *Step -- so a caller outside this package's request/response
// handler (a scheduled job, say) can present a provider's credential the same
// way, with the same cache, retry and redaction behaviour, instead of
// building a second implementation.
package common

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
)

func TestAuthenticatorAppliesAHeaderCredentialStandalone(t *testing.T) {
	t.Setenv("STANDALONE_HEADER_VALUE", "secret-value")
	auth := NewAuthenticator(AuthProfile{
		Provider: "standalone", Scheme: util.AuthSchemeHeader,
		HeaderName: "x-api-key", HeaderValueEnv: "STANDALONE_HEADER_VALUE",
	})

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.invalid/x", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if err := auth.Apply(context.Background(), http.DefaultClient, 1<<20, req); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := req.Header.Get("x-api-key"); got != "secret-value" {
		t.Fatalf("x-api-key = %q, want secret-value", got)
	}
}

// The token exchange, cache and forget all work with a client the caller
// supplies -- no Step, no step-wide config -- and the standalone entry point
// is held to the exact same TTL/skew/redaction rules as Step's own callers.
func TestAuthenticatorExchangesCachesAndForgetsATokenStandalone(t *testing.T) {
	t.Setenv("STANDALONE_TOKEN_USER", "user-1")
	t.Setenv("STANDALONE_TOKEN_SECRET", "secret-1")

	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"token":"tok-` + time.Now().Format("150405.000") + `"}`))
	}))
	defer server.Close()

	auth := NewAuthenticator(AuthProfile{
		Provider: "standalone", Scheme: util.AuthSchemeTokenQuery,
		TokenURL: server.URL, TokenUserField: "access_name", TokenUserEnv: "STANDALONE_TOKEN_USER",
		TokenSecretName: "password", TokenSecretEnv: "STANDALONE_TOKEN_SECRET",
		TokenResponseField: "token", QueryName: "token", TokenTTLRaw: "1h",
	})
	if err := (&auth.cfg).validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	client := server.Client()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.invalid/x", nil)
	if err := auth.Apply(context.Background(), client, 1<<20, req); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	first := req.URL.Query().Get("token")
	if first == "" {
		t.Fatal("no token placed in the query")
	}
	if calls != 1 {
		t.Fatalf("exchanged %d times, want 1", calls)
	}

	// Reused within TTL: a second Apply must not re-exchange.
	req2, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.invalid/y", nil)
	if err := auth.Apply(context.Background(), client, 1<<20, req2); err != nil {
		t.Fatalf("Apply (second): %v", err)
	}
	if calls != 1 {
		t.Fatalf("exchanged %d times on the second call, want 1 (cached)", calls)
	}

	forms := auth.SecretForms()
	found := false
	for _, f := range forms {
		found = found || f == first
	}
	if !found {
		t.Fatalf("SecretForms = %v, want it to include the held token %q", forms, first)
	}

	// Forget drops the cache; the next Apply exchanges again.
	auth.Forget()
	req3, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.invalid/z", nil)
	if err := auth.Apply(context.Background(), client, 1<<20, req3); err != nil {
		t.Fatalf("Apply (after Forget): %v", err)
	}
	if calls != 2 {
		t.Fatalf("exchanged %d times after Forget, want 2", calls)
	}
}
