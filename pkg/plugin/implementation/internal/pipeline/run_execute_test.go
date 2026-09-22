package pipeline

// run_execute_test.go covers the half of a run that run_test.go stops short
// of: everything after the schedule says "go", with a collector that succeeds.
//
// The collector is fake, so what is under test is the frame's own work --
// exchanging a token, giving the pipeline a directory, writing what came back,
// clearing what a previous run left. A real collector's fetching is tested in
// the capability package that owns it.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

// tokenUpstream answers the one call the frame itself makes before handing
// over to a collector.
func tokenUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"token":"tok-fake"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func upstreamEnv(baseURL string) func(string) (string, bool) {
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

// firedAt is an instant the fixture's midnight schedule has already fired for.
func firedAt(t *testing.T) time.Time {
	t.Helper()
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	return time.Date(2026, 9, 21, 6, 0, 0, 0, ist)
}

func TestRunWritesWhatTheCollectorReturned(t *testing.T) {
	upstream := tokenUpstream(t)
	outDir := t.TempDir()
	runLog := &fakeRunLog{}
	now := firedAt(t)

	collector := newFakeCollector()
	collector.result = CollectResult{
		Catalogues: []Catalogue{
			{Slug: "MH", CatalogID: "catalog:example:MH", Content: []byte(`{"a":1}`)},
			{Slug: "KA", CatalogID: "catalog:example:KA", Content: []byte(`{"a":2}`)},
		},
		Counters: map[string]int{"things": 7},
	}

	report, err := Run(context.Background(), RunOptions{
		Collector: collector,
		Record:    publishingRecord(),
		RunLog:    runLog,
		Lookup:    upstreamEnv(upstream.URL),
		Now:       now,
		OutDir:    outDir,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if collector.calls != 1 {
		t.Errorf("collected %d times, want 1", collector.calls)
	}
	if len(report.Catalogues) != 2 {
		t.Errorf("Catalogues = %d, want 2", len(report.Catalogues))
	}
	if report.Counters["things"] != 7 {
		t.Errorf("the collector's counters did not reach the report: %v", report.Counters)
	}

	// The fixture's metadata.name is what names the files.
	for _, slug := range []string{"MH", "KA"} {
		path := filepath.Join(report.OutDir, "example-"+slug+".json")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("catalogue for %s was not written: %v", slug, err)
		}
	}

	// Publish defaults to off, so nothing may have been sent.
	if report.Published != nil {
		t.Error("a run with Publish unset reported a publish result")
	}
	if len(runLog.recorded) != 1 {
		t.Errorf("recorded %d runs, want 1", len(runLog.recorded))
	}
}

// The corruption this design exists to prevent: two DIFFERENT pipelines
// pointed at one configured directory must not delete or publish each other's
// catalogues.
//
// Both halves of that risk are real here. The stale sweep runs before every
// collection and deletes by filename prefix, and the publish step globs by the
// same prefix -- so if both pipelines wrote into the shared directory, the
// second run would erase the first's catalogues and then post whatever was
// left as its own. It would look like a healthy run, on both sides.
func TestRunKeepsEachPipelineInItsOwnDirectory(t *testing.T) {
	upstream := tokenUpstream(t)
	shared := t.TempDir()
	now := firedAt(t)

	run := func(collector *fakeCollector) RunReport {
		t.Helper()
		files := collector.Pipeline()
		record := publishingRecord()
		record.Actions["publish"] = model.ActionPlan{Mappings: files.RegistryPath}

		collector.result = CollectResult{Catalogues: []Catalogue{
			{Slug: "MH", CatalogID: "catalog:" + collector.Capability() + ":MH", Content: []byte(`{"a":1}`)},
		}}

		report, err := Run(context.Background(), RunOptions{
			Collector: collector,
			Record:    record,
			Lookup:    upstreamEnv(upstream.URL),
			Now:       now,
			OutDir:    shared,
		})
		if err != nil {
			t.Fatalf("Run(%s): %v", collector.Capability(), err)
		}
		return report
	}

	first := run(newFakeCollector())

	// The second pipeline: a different capability, a different file, a
	// different metadata.name -- exactly the shape a real second capability
	// has.
	other := &fakeCollector{
		capability: "example:Other",
		files: Files{
			FS:           fakeFiles,
			Path:         "testdata/other.yaml",
			RegistryPath: "pkg/plugin/implementation/Other/cataloguepublish-other/testdata/other.yaml",
		},
	}
	second := run(other)

	if first.OutDir == second.OutDir {
		t.Fatalf("both pipelines wrote to %s; each would sweep away the other's catalogues", first.OutDir)
	}
	if filepath.Dir(first.OutDir) != shared || filepath.Dir(second.OutDir) != shared {
		t.Errorf("directories %q and %q are not both beneath %q", first.OutDir, second.OutDir, shared)
	}

	// The first pipeline's catalogue must have survived the second's run --
	// this is the assertion that actually fails if the sweep is pointed at a
	// shared directory.
	if _, err := os.Stat(filepath.Join(first.OutDir, "example-MH.json")); err != nil {
		t.Errorf("the first pipeline's catalogue did not survive the second's run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(second.OutDir, "other-MH.json")); err != nil {
		t.Errorf("the second pipeline's catalogue was not written: %v", err)
	}
}

// A collector that fails must fail the run, and the run must not be recorded:
// a transient upstream outage at midnight is retried on the next tick rather
// than costing the whole day.
func TestRunDoesNotRecordAFailedCollection(t *testing.T) {
	upstream := tokenUpstream(t)
	runLog := &fakeRunLog{}

	collector := newFakeCollector()
	collector.err = context.DeadlineExceeded

	_, err := Run(context.Background(), RunOptions{
		Collector: collector,
		Record:    publishingRecord(),
		RunLog:    runLog,
		Lookup:    upstreamEnv(upstream.URL),
		Now:       firedAt(t),
		OutDir:    t.TempDir(),
	})
	if err == nil {
		t.Fatal("a failed collection reported success")
	}
	if len(runLog.recorded) != 0 {
		t.Error("a failed run was recorded; the day's retry is now lost")
	}
}

// A pipeline file declaring a different capability than the collector claims
// would key the run log and the output directory on one name while publishing
// under another.
func TestRunRefusesACollectorThatDisagreesWithItsPipeline(t *testing.T) {
	collector := newFakeCollector()
	collector.capability = "example:SomethingElse"

	_, err := Run(context.Background(), RunOptions{
		Collector: collector,
		Record:    publishingRecord(),
		Lookup:    upstreamEnv("http://unused.invalid"),
		Now:       firedAt(t),
	})
	if err == nil {
		t.Fatal("a collector serving one capability ran another's pipeline")
	}
	assertNotCollected(t, collector)
}

func TestRunRefusesWithoutACollector(t *testing.T) {
	if _, err := Run(context.Background(), RunOptions{Record: publishingRecord()}); err == nil {
		t.Fatal("a run with no collector was attempted")
	}
}
