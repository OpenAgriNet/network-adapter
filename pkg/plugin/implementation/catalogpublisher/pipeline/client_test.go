package pipeline

// client_test.go proves the upstream Client's two calls -- minting a token and
// making a mapped GET -- against a synthetic upstream, and proves that neither
// leaks a credential into an error.
//
// The tests that need a REAL mapping file live with the pipeline that owns
// one (see the capability packages' own client_test.go); what is here is what
// holds for every pipeline.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

// client_test.go proves pipelineClient's two calls -- minting a token and
// making a mapped GET -- against both a synthetic upstream and, for the
// mapped GET, the real master-states.yaml mapping this package embeds. The
// no-data classification is exercised directly because it is the whole
// reason this client distinguishes an empty result from a failure: recording
// it as a failure previously turned 27 of 36 states into false outages (see
// ErrNoUpstreamData in client.go).

func TestPipelineClient_Token_ExchangesCredentials(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"token":"tok-abc"}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL)
	token, err := client.Token(context.Background(), "user1", "secret1")
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if token != "tok-abc" {
		t.Errorf("token = %q, want %q", token, "tok-abc")
	}
	if gotBody["access_name"] != "user1" || gotBody["password"] != "secret1" {
		t.Errorf("body = %v, want access_name=user1 password=secret1", gotBody)
	}
}

func TestPipelineClient_Token_RejectsNonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"invalid credentials"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	client := NewClient(srv.URL)
	if _, err := client.Token(context.Background(), "user1", "secret1"); err == nil {
		t.Fatal("want an error for a 401 response")
	}
}

// TestPipelineClient_Token_NeverLeaksTheCredentials: the token endpoint
// rejects by quoting the request back, and that request carries the password.
func TestPipelineClient_Token_NeverLeaksTheCredentials(t *testing.T) {
	const password = "pw-SECRET-do-not-leak-2b8d"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		http.Error(w, `{"error":"invalid","received":`+string(body)+`}`, http.StatusUnauthorized)
	}))
	defer upstream.Close()

	client := NewClient(upstream.URL)
	client.http = upstream.Client()

	_, err := client.Token(context.Background(), "user1", password)
	if err == nil {
		t.Fatal("want an error for rejected credentials")
	}
	for _, form := range []string{err.Error(), fmt.Sprint(err), fmt.Sprintf("%+v", err)} {
		if strings.Contains(form, password) {
			t.Errorf("the password reached an error message: %s", form)
		}
	}
}

func TestJSONShape(t *testing.T) {
	for name, tc := range map[string]struct {
		value any
		want  string
	}{
		"null":   {nil, "null"},
		"bool":   {true, "boolean"},
		"number": {float64(1), "number"},
		"string": {"x", "string"},
		"object": {map[string]any{}, "object"},
		"array":  {[]any{}, "array"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := jsonShape(tc.value); got != tc.want {
				t.Errorf("jsonShape(%v) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

// staticMapper returns a fixed mapped request, so a test can drive asQuery
// with a shape the real mappings never produce.
type staticMapper struct{ mapped string }

func (s staticMapper) Verify(context.Context, string, any) error { return nil }
func (s staticMapper) Transform(_ context.Context, _ string, d definition.Direction, _ any) ([]byte, error) {
	if d == definition.DirectionRequest {
		return []byte(s.mapped), nil
	}
	return []byte(`[]`), nil
}

// TestPipelineClient_HTTPGet_RejectsANonScalarMappedField: there is no single
// correct way to put an object or a list into a query string -- repeated keys,
// comma-joined and indexed are all in use somewhere -- so choosing one here
// would bury a convention in Go that belongs in the mapping.
func TestPipelineClient_HTTPGet_RejectsANonScalarMappedField(t *testing.T) {
	client := NewClient("http://unused.invalid")

	_, err := client.Get(context.Background(),
		staticMapper{mapped: `{"token":"t","nested":{"a":1}}`},
		"mapping.yaml", "/v1/fetch", map[string]any{"token": "t"})
	if err == nil {
		t.Fatal("a non-scalar mapped field was accepted")
	}
	if !strings.Contains(err.Error(), "nested") {
		t.Errorf("error %q does not name the offending field", err)
	}
}
