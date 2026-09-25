package pipeline

// fixture_test.go gives the frame a real pipeline of its own to run.
//
// It is a YAML in testdata/, not a Go stub, and that is deliberate: a stub
// would agree with the interpreter by construction, which is precisely what
// the interpreter must not be tested against. The fixture exercises the same
// constructs a capability uses -- an auth exchange, a step with a mapping, a
// catalogue block -- so the frame is tested through the path it really runs.

import (
	"embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

//go:embed testdata/*.yaml testdata/mappings/*.yaml
var fixtureFS embed.FS

const (
	fixtureCapability   = "example:Thing"
	fixturePipelinePath = "testdata/minimal.yaml"
	fixtureRegistryPath = "pkg/plugin/implementation/Example/cataloguepublish-example/testdata/minimal.yaml"
)

// fixturePipeline is the Files a run is given.
func fixturePipeline() Files {
	return Files{FS: fixtureFS, Path: fixturePipelinePath, RegistryPath: fixtureRegistryPath}
}

// otherPipeline is a SECOND capability, for the tests about two pipelines not
// treading on each other.
func otherPipeline() Files {
	return Files{
		FS:           fixtureFS,
		Path:         "testdata/other.yaml",
		RegistryPath: "pkg/plugin/implementation/Other/cataloguepublish-other/testdata/other.yaml",
	}
}

// fixtureUpstream serves the token exchange and the `things` call, and counts
// every request so a test can prove a refused run never reached it.
type fixtureUpstream struct {
	*httptest.Server
	calls atomic.Int64
}

// newFixtureUpstream serves rows as the `things` response body.
func newFixtureUpstream(t *testing.T, rows string) *fixtureUpstream {
	t.Helper()
	upstream := &fixtureUpstream{}
	upstream.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream.calls.Add(1)
		switch r.URL.Path {
		case "/token":
			_, _ = w.Write([]byte(`{"token":"tok-fixture"}`))
		case "/things":
			_, _ = w.Write([]byte(rows))
		default:
			t.Errorf("unexpected upstream path %q", r.URL.Path)
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

// twoGroupsOfThings is the healthy fixture body: two groups, one of which has
// enough rows to split across chunks at the fixture's budget of 2.
const twoGroupsOfThings = `[
  {"thing_id":1,"group_code":"AA","size":10},
  {"thing_id":2,"group_code":"AA","size":20},
  {"thing_id":3,"group_code":"AA","size":30},
  {"thing_id":4,"group_code":"BB","size":40}
]`

// fixtureEnv points a run at the fixture upstream and supplies the credentials
// the run refuses to start without.
func fixtureEnv(baseURL string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		switch name {
		case "EXAMPLE_BASE_URL":
			return baseURL, true
		case "EXAMPLE_TOKEN_USER":
			return "test-user", true
		case "EXAMPLE_TOKEN_SECRET":
			return "test-secret", true
		case "EXAMPLE_PUBLISH_URL":
			return "http://publish.invalid", true
		}
		return "", false
	}
}

// assertNeverCalled is the assertion the decision tests share: the run refused
// or stood down, so no upstream was touched.
func assertNeverCalled(t *testing.T, upstream *fixtureUpstream) {
	t.Helper()
	if calls := upstream.calls.Load(); calls != 0 {
		t.Errorf("the upstream was called %d time(s); this run should not have reached it at all", calls)
	}
}

// decodeFixtureCatalogue reads what testdata/mappings/catalog.yaml renders.
type fixtureCatalogue struct {
	Slug  string  `json:"slug"`
	Count float64 `json:"count"`
	IDs   []any   `json:"ids"`
}

func decodeFixtureCatalogue(t *testing.T, content []byte) fixtureCatalogue {
	t.Helper()
	var out fixtureCatalogue
	if err := json.Unmarshal(content, &out); err != nil {
		t.Fatalf("the rendered catalogue did not decode: %v\nbody: %s", err, content)
	}
	return out
}
