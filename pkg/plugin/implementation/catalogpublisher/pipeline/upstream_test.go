package pipeline

// client_test.go covers the parts of the upstream Client that hold for every
// pipeline: how a mapped request is turned into a query, and what it refuses.
//
// The token exchange is in auth_test.go, because it is declared by the
// pipeline file rather than known here. Tests needing a REAL mapping live with
// the capability that owns one.

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

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

// One line per upstream call, and the credential is never in it -- not in the
// path (the query is stripped), not in a field (Review Focus 3).
func TestClientLogsEachCallWithoutTheToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()

	var buf bytes.Buffer
	client := NewClient(server.URL).
		WithLogger(slog.New(slog.NewTextHandler(&buf, nil))).
		WithCredential(Credential{Value: "s3cret-token", CarriedAs: "query", Name: "token"})
	ctx := withLogItem(context.Background(), "MH")
	if _, err := client.fetchGet(ctx, "/v1/data", "token=s3cret-token&x=1"); err != nil {
		t.Fatalf("GET: %v", err)
	}
	if _, err := client.fetchPost(ctx, "/v1/search", []byte(`{}`)); err != nil {
		t.Fatalf("POST: %v", err)
	}
	out := buf.String()
	if got := strings.Count(out, "upstream call"); got != 2 {
		t.Errorf("logged %d call lines, want 2:\n%s", got, out)
	}
	for _, want := range []string{"method=GET", "path=/v1/data", "method=POST", "path=/v1/search",
		"status=200", "duration=", "bytes=2", "item=MH"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "s3cret-token") {
		t.Fatalf("the token reached the log:\n%s", out)
	}
}

// A call that never reaches the upstream is logged too, with no status.
func TestClientLogsAnUnreachableCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()

	var buf bytes.Buffer
	client := NewClient(url).WithLogger(slog.New(slog.NewTextHandler(&buf, nil)))
	if _, err := client.fetchGet(context.Background(), "/v1/data", ""); err == nil {
		t.Fatal("a closed server answered")
	}
	if !strings.Contains(buf.String(), "status=0") {
		t.Errorf("an unreachable call was not logged with status 0:\n%s", buf.String())
	}
}
