package pipeline

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// A pipeline is fetched from the URL the registry names, validated against
// the contract it declares, and parsed.
func TestLoadPipelineFetchesAndValidates(t *testing.T) {
	spec, err := loadPipeline(context.Background(), fixturePipeline())
	if err != nil {
		t.Fatalf("loadPipeline: %v", err)
	}
	if spec.Metadata.Capability != fixtureCapability {
		t.Fatalf("capability = %q, want %q", spec.Metadata.Capability, fixtureCapability)
	}
}

// Only https, except on loopback (a developer's server, these tests): the
// pipeline decides where credentials are sent, so it must not travel in the
// clear or come from a file scheme a registry record could point anywhere.
func TestCheckPipelineURL(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://raw.githubusercontent.com/org/repo/main/agmarknet.yaml":     true,
		"http://127.0.0.1:8080/agmarknet.yaml":                               true,
		"http://localhost/agmarknet.yaml":                                    true,
		"http://example.org/agmarknet.yaml":                                  false,
		"file:///etc/passwd":                                                 false,
		"pkg/plugin/implementation/MandiPrice/catalogpublish/agmarknet.yaml": false,
		"":                      false,
		"https:///no-host.yaml": false,
	} {
		err := checkPipelineURL(raw)
		if ok && err != nil {
			t.Errorf("checkPipelineURL(%q) = %v, want accepted", raw, err)
		}
		if !ok && err == nil {
			t.Errorf("checkPipelineURL(%q) accepted, want refused", raw)
		}
	}
}

// A repo path in the registry -- what records held before -- is refused with
// a message that says what to put there instead.
func TestARepoPathSaysAURLIsNeeded(t *testing.T) {
	err := checkPipelineURL("pkg/plugin/implementation/MandiPrice/catalogpublish/agmarknet.yaml")
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("err = %v; want it to say the registry must name an https URL", err)
	}
}

// If the host is down at midnight, the last copy that loaded is used, so a
// hosting outage does not cost the day. The first load has no copy and fails.
func TestLoadPipelineFallsBackToTheLastGoodCopy(t *testing.T) {
	var down atomic.Bool
	body, err := fixtureFS.ReadFile(fixturePipelinePath)
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(body)
	}))
	defer server.Close()
	files := Files{URL: server.URL + "/fallback-" + t.Name() + ".yaml"}

	down.Store(true)
	if _, err := loadPipeline(context.Background(), files); err == nil {
		t.Fatal("a first load from a host that is down succeeded")
	}
	down.Store(false)
	if _, err := loadPipeline(context.Background(), files); err != nil {
		t.Fatalf("load while up: %v", err)
	}
	down.Store(true)
	spec, err := loadPipeline(context.Background(), files)
	if err != nil {
		t.Fatalf("load while down after a good load: %v; want the last good copy", err)
	}
	if spec.Metadata.Capability != fixtureCapability {
		t.Errorf("fallback spec capability = %q", spec.Metadata.Capability)
	}
}

// A file that does not match its contract is refused even when fetched, and
// is NOT kept as a last-good copy.
func TestLoadPipelineRefusesAnInvalidFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("schemaRef: {uses: publish.oan/CatalogPipeline/v1}\nnot_a_key: 1\n"))
	}))
	defer server.Close()
	if _, err := loadPipeline(context.Background(), Files{URL: server.URL + "/bad.yaml"}); err == nil {
		t.Fatal("an invalid pipeline file was accepted")
	}
}

// A file's mapping references resolve relative to the file's own URL, the
// way a browser resolves a link.
func TestResolveMappingRef(t *testing.T) {
	base := "https://host/org/repo/main/MandiPrice/catalogpublish/agmarknet.yaml"
	for ref, want := range map[string]string{
		"mappings/catalog.yaml":    "https://host/org/repo/main/MandiPrice/catalogpublish/mappings/catalog.yaml",
		"./mappings/catalog.yaml":  "https://host/org/repo/main/MandiPrice/catalogpublish/mappings/catalog.yaml",
		"../shared/catalog.yaml":   "https://host/org/repo/main/MandiPrice/shared/catalog.yaml",
		"https://other/x/map.yaml": "https://other/x/map.yaml",
	} {
		got, err := resolveMappingRef(base, ref)
		if err != nil || got != want {
			t.Errorf("resolveMappingRef(%q) = %q, %v; want %q", ref, got, err, want)
		}
	}
}

// Whoever edits a pipeline file must not be able to choose where the
// credentials go: the upstream address comes from inputs.baseUrl (env or
// plugin config), never a literal host written in the file.
func TestAPipelineNamingALiteralUpstreamHostIsRefused(t *testing.T) {
	spec := Spec{Upstream: Upstream{BaseURL: "https://collector.example/steal"}}
	if err := checkUpstreamIsAnInput(spec); err == nil || !strings.Contains(err.Error(), "${inputs.baseUrl}") {
		t.Fatalf("err = %v; want a literal upstream host refused, naming ${inputs.baseUrl}", err)
	}
	if err := checkUpstreamIsAnInput(Spec{Upstream: Upstream{BaseURL: "${inputs.baseUrl}"}}); err != nil {
		t.Errorf("the input reference was refused: %v", err)
	}
	if err := checkUpstreamIsAnInput(Spec{}); err != nil {
		t.Errorf("a pipeline with no upstream was refused: %v", err)
	}
}

// A call path must start with "/": appended to https://host, "@evil.example/x"
// would make the request -- credential and all -- go to evil.example.
func TestAPathThatWouldChangeTheHostIsRefused(t *testing.T) {
	var reached atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Store(true)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()
	client := NewClient(server.URL)
	for _, path := range []string{"@evil.example/x", "evil", ".evil.example/x"} {
		if _, err := client.fetchGet(context.Background(), path, ""); err == nil {
			t.Errorf("GET path %q was sent", path)
		}
		if _, err := client.fetchPost(context.Background(), path, []byte(`{}`)); err == nil {
			t.Errorf("POST path %q was sent", path)
		}
	}
	if reached.Load() {
		t.Error("a refused path still reached a server")
	}
}

// RemotePipeline is the crawler's resolver: the registry's value, checked,
// becomes the Files a run is given. Nothing is fetched until the run.
func TestRemotePipelineChecksTheRegistryValue(t *testing.T) {
	files, err := RemotePipeline(" https://host/MandiPrice/catalogpublish/agmarknet.yaml ")
	if err != nil || files.URL != "https://host/MandiPrice/catalogpublish/agmarknet.yaml" {
		t.Fatalf("RemotePipeline = %+v, %v", files, err)
	}
	if _, err := RemotePipeline("pkg/plugin/implementation/MandiPrice/catalogpublish/agmarknet.yaml"); err == nil {
		t.Fatal("a repo path was accepted as a pipeline URL")
	}
}
