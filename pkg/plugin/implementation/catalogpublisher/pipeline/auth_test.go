package pipeline

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An unset kind is a token exchange. No existing file names its kind, and
// none may have to start.
func TestAuthenticatorForDefaultsToTokenExchange(t *testing.T) {
	a, err := authenticatorFor(Auth{})
	if err != nil {
		t.Fatalf("authenticatorFor: %v", err)
	}
	if got := strings.Join(a.RequiredInputs(), ","); got != "tokenUser,tokenSecret" {
		t.Fatalf("RequiredInputs = %q, want tokenUser,tokenSecret", got)
	}
}

func TestAuthenticatorForRefusesAnUnknownKind(t *testing.T) {
	if _, err := authenticatorFor(Auth{Kind: "basic"}); err == nil {
		t.Fatal("kind basic was accepted; only tokenExchange, none and apiKey exist")
	}
}

func TestNoneNeedsNothingAndCarriesNothing(t *testing.T) {
	a, err := authenticatorFor(Auth{Kind: "none"})
	if err != nil {
		t.Fatalf("authenticatorFor: %v", err)
	}
	if len(a.RequiredInputs()) != 0 {
		t.Fatalf("none requires %v", a.RequiredInputs())
	}
	cred, err := a.Prepare(context.Background(), NewClient("https://example.invalid"), newRunContext(nil, ""))
	if err != nil || cred.Value != "" {
		t.Fatalf("Prepare = %+v, %v; want an empty credential", cred, err)
	}
}

// apiKey makes no round trip: the value is an input, placed where the file says.
func TestAPIKeyResolvesItsValueWithoutARoundTrip(t *testing.T) {
	a, err := authenticatorFor(Auth{Kind: "apiKey", Token: AuthTokenSpec{
		Value: "${inputs.apiKey}", CarriedAs: "header", Name: "x-api-key",
	}})
	if err != nil {
		t.Fatalf("authenticatorFor: %v", err)
	}
	rc := newRunContext(map[string]string{"apiKey": "k-123"}, "")
	cred, err := a.Prepare(context.Background(), NewClient("https://example.invalid"), rc)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	want := Credential{Value: "k-123", CarriedAs: "header", Name: "x-api-key"}
	if cred != want {
		t.Fatalf("credential = %+v, want %+v", cred, want)
	}
}

// An apiKey that does not say where it rides is refused rather than guessed.
func TestAPIKeyWithoutPlacementIsRefused(t *testing.T) {
	a, err := authenticatorFor(Auth{Kind: "apiKey", Token: AuthTokenSpec{Value: "${inputs.apiKey}"}})
	if err != nil {
		t.Fatalf("authenticatorFor: %v", err)
	}
	rc := newRunContext(map[string]string{"apiKey": "k-123"}, "")
	if _, err := a.Prepare(context.Background(), NewClient("https://example.invalid"), rc); err == nil {
		t.Fatal("an apiKey with no carriedAs/name was accepted")
	}
}

// Review Focus 4: a query-carried key on GET is the mapping's to place; the
// client adds it only on POST, so it is never sent twice.
func TestApplyCredentialPlacesQueryKeysOnlyOnPost(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.RawQuery+" h="+r.Header.Get("x-api-key"))
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()

	client := NewClient(server.URL).WithCredential(Credential{Value: "k", CarriedAs: "query", Name: "key"})
	if _, err := client.fetchGet(context.Background(), "/g", "key=k"); err != nil {
		t.Fatalf("GET: %v", err)
	}
	if _, err := client.fetchPost(context.Background(), "/p", []byte(`{}`)); err != nil {
		t.Fatalf("POST: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("server saw %d calls, want 2", len(seen))
	}
	if seen[0] != "GET key=k h=" {
		t.Errorf("GET sent %q; the mapping's query must be sent once, untouched", seen[0])
	}
	if seen[1] != "POST key=k h=" {
		t.Errorf("POST sent %q; want the key added to the query", seen[1])
	}
}

func TestApplyCredentialPlacesHeadersEverywhere(t *testing.T) {
	var headers []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = append(headers, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()

	client := NewClient(server.URL).WithCredential(Credential{
		Value: "t", CarriedAs: "header", Name: "Authorization", Prefix: "Bearer ",
	})
	if _, err := client.fetchGet(context.Background(), "/g", ""); err != nil {
		t.Fatalf("GET: %v", err)
	}
	if _, err := client.fetchPost(context.Background(), "/p", []byte(`{}`)); err != nil {
		t.Fatalf("POST: %v", err)
	}
	if len(headers) != 2 {
		t.Fatalf("server saw %d calls, want 2", len(headers))
	}
	for i, h := range headers {
		if h != "Bearer t" {
			t.Errorf("call %d Authorization = %q, want %q", i, h, "Bearer t")
		}
	}
}
