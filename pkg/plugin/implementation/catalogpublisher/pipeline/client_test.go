package pipeline

// client_test.go covers the parts of the upstream Client that hold for every
// pipeline: how a mapped request is turned into a query, and what it refuses.
//
// The token exchange is in auth_test.go, because it is declared by the
// pipeline file rather than known here. Tests needing a REAL mapping live with
// the capability that owns one.

import (
	"context"
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
