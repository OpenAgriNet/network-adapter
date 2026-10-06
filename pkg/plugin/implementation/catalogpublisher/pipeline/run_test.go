package pipeline

// run_test.go covers the parts of a run that must be decided BEFORE anything
// is fetched or published. The fetching itself is covered step by step
// elsewhere (client_test, catalog_test, publish_test) and against the real
// services in live_test; what is tested here is that a run which should not
// happen does not happen at all.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"gopkg.in/yaml.v3"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"

	"github.com/beckn/catalog-core/pkg/catalog/crawler"
)

// publishingRecord is the live registry's answer, in the shape the gate reads.
func publishingRecord() *model.ProviderRecord {
	return &model.ProviderRecord{
		BindingKey: "exampleco|" + fixtureCapability,
		Actions: map[string]model.ActionPlan{
			"select":  {Method: "GET", Path: "/v1/fetch"},
			"publish": {Mappings: fixtureRegistryPath},
		},
	}
}

// fakeRunLog records what a run asked of it, so a test can prove a run that
// did not happen also did not claim to have happened.
type fakeRunLog struct {
	last      time.Time
	readErr   error
	writeErr  error
	recorded  []time.Time
	readCalls int
}

func (f *fakeRunLog) LastPipelineRun(context.Context, string) (time.Time, error) {
	f.readCalls++
	return f.last, f.readErr
}

func (f *fakeRunLog) RecordPipelineRun(_ context.Context, _ string, at time.Time) error {
	f.recorded = append(f.recorded, at)
	return f.writeErr
}

// An upstream address that cannot be reached, so any test whose run reaches
// the fetching stage fails loudly there instead of quietly going to a real
// service.
func unreachableEnv(name string) (string, bool) {
	switch name {
	case "EXAMPLE_BASE_URL":
		// Reserved for documentation examples (RFC 2606) and resolvable by
		// nothing.
		return "http://upstream.invalid", true
	case "EXAMPLE_TOKEN_USER":
		return "test-user", true
	case "EXAMPLE_TOKEN_SECRET":
		return "test-secret", true
	case "EXAMPLE_PUBLISH_URL":
		return "http://publish.invalid", true
	}
	return "", false
}

// The whole reason the run log exists: a restart minutes after a run must not
// publish the day's catalogs a second time.
func TestRunSkipsWhenTheRunLogSaysItAlreadyRanThisFiring(t *testing.T) {
	upstream := newFixtureUpstream(t, twoGroupsOfThings)
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	// Fired at 00:00 IST, ran at 00:01, restarted at 00:05.
	log := &fakeRunLog{last: time.Date(2026, 9, 21, 0, 1, 0, 0, ist)}

	report, err := Run(context.Background(), RunOptions{
		Record:   publishingRecord(),
		Pipeline: fixturePipeline(),
		RunLog:   log,
		Now:      time.Date(2026, 9, 21, 0, 5, 0, 0, ist),
		Lookup:   fixtureEnv(upstream.URL),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Due {
		t.Fatal("ran again five minutes after running; this publishes twice")
	}
	if len(log.recorded) != 0 {
		t.Errorf("a run that did not happen recorded %d run(s)", len(log.recorded))
	}
	if report.Reason == "" {
		t.Error("no reason given for doing nothing")
	}
	assertNeverCalled(t, upstream)
}

// A capability the registry never sanctioned must fail before any work, and
// must not leave a run recorded against it.
func TestRunRefusesACapabilityThatDoesNotPublish(t *testing.T) {
	upstream := newFixtureUpstream(t, twoGroupsOfThings)
	log := &fakeRunLog{}
	_, err := Run(context.Background(), RunOptions{
		Pipeline: fixturePipeline(),
		Record: &model.ProviderRecord{
			BindingKey: "mausamgram|openagrinet:WeatherObservation",
			Actions:    map[string]model.ActionPlan{"select": {Method: "GET"}},
		},
		RunLog: log,
		Lookup: fixtureEnv(upstream.URL),
	})
	if err == nil {
		t.Fatal("a select-only capability produced a run")
	}
	assertNeverCalled(t, upstream)
	if len(log.recorded) != 0 {
		t.Error("a refused run was recorded as having run")
	}
}

// If the run log cannot be read, whether today's run already happened is
// unknown. Running anyway is a guess, and the guess that publishes is the
// wrong one to make silently.
func TestRunRefusesWhenTheRunLogCannotBeRead(t *testing.T) {
	upstream := newFixtureUpstream(t, twoGroupsOfThings)
	log := &fakeRunLog{readErr: errors.New("database is down")}
	_, err := Run(context.Background(), RunOptions{
		Record:   publishingRecord(),
		Pipeline: fixturePipeline(),
		RunLog:   log,
		Lookup:   fixtureEnv(upstream.URL),
	})
	if err == nil {
		t.Fatal("an unreadable run log was treated as 'never ran'")
	}
	assertNeverCalled(t, upstream)
	if len(log.recorded) != 0 {
		t.Error("a refused run was recorded as having run")
	}
}

// Without a run log there is nothing to consult, so every tick is due. That is
// the pre-persistence behaviour and it is a foot-gun, so the report has to say
// so rather than look identical to a persisted run.
func TestRunWithoutARunLogSaysSoInTheReport(t *testing.T) {
	upstream := newFixtureUpstream(t, twoGroupsOfThings)
	report, err := Run(context.Background(), RunOptions{
		Record:   publishingRecord(),
		Pipeline: fixturePipeline(),
		Lookup:   fixtureEnv(upstream.URL),
		DryRun:   true,
		Publish:  false,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Due {
		t.Fatalf("not due with no run log: %s", report.Reason)
	}
	if !report.Unpersisted {
		t.Error("report does not flag that this run's outcome is not persisted")
	}
}

// DryRun stops after the decision. It exists so an operator can ask "would
// this run, and with what" against production config without fetching
// anything.
func TestRunDryRunDoesNoWork(t *testing.T) {
	upstream := newFixtureUpstream(t, twoGroupsOfThings)
	log := &fakeRunLog{}
	report, err := Run(context.Background(), RunOptions{
		Record:   publishingRecord(),
		Pipeline: fixturePipeline(),
		RunLog:   log,
		Lookup:   fixtureEnv(upstream.URL),
		DryRun:   true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Due {
		t.Fatalf("not due: %s", report.Reason)
	}
	if report.PipelinePath == "" {
		t.Error("report does not say which pipeline it resolved")
	}
	// Nothing ran, so nothing may be recorded as having run.
	if len(log.recorded) != 0 {
		t.Errorf("a dry run recorded %d run(s)", len(log.recorded))
	}
	assertNeverCalled(t, upstream)
}

// Missing credentials are a configuration error, not something to discover
// halfway through a fetch.
func TestRunRefusesIncompleteConfiguration(t *testing.T) {
	upstream := newFixtureUpstream(t, twoGroupsOfThings)
	_, err := Run(context.Background(), RunOptions{
		Record:   publishingRecord(),
		Pipeline: fixturePipeline(),
		Lookup:   func(string) (string, bool) { return "", false },
	})
	if err == nil {
		t.Fatal("a run with no upstream credentials was attempted")
	}
	assertNeverCalled(t, upstream)
}

// ---- the run's full execution (formerly run_execute_test.go) ----

// firedAt is an instant the fixture's midnight schedule has already fired for.
func firedAt(t *testing.T) time.Time {
	t.Helper()
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	return time.Date(2026, 9, 21, 6, 0, 0, 0, ist)
}

// The whole path: token, step, mapping, grouping, chunking, render, write.
//
// The fixture has four things in two groups and a chunk budget of 2, so group
// AA (three things) must split into two catalogs and BB must not.
func TestRunExecutesTheFileAndWritesWhatItDescribes(t *testing.T) {
	upstream := newFixtureUpstream(t, twoGroupsOfThings)
	outDir := t.TempDir()
	runLog := &fakeRunLog{}

	report, err := Run(context.Background(), RunOptions{
		Pipeline: fixturePipeline(),
		Record:   publishingRecord(),
		RunLog:   runLog,
		Lookup:   fixtureEnv(upstream.URL),
		Now:      firedAt(t),
		OutDir:   outDir,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// AA splits at the budget, BB does not: three catalogs in all.
	if len(report.Catalogs) != 3 {
		t.Fatalf("got %d catalogs, want 3 (AA, AA-2, BB): %+v", len(report.Catalogs), report.Catalogs)
	}

	bySlug := map[string]BuiltCatalog{}
	for _, catalog := range report.Catalogs {
		bySlug[catalog.Slug] = catalog
	}
	for _, slug := range []string{"AA", "AA-2", "BB"} {
		catalog, ok := bySlug[slug]
		if !ok {
			t.Fatalf("no catalog for %s; got %v", slug, report.Catalogs)
		}
		if want := "catalog:example:" + slug; catalog.CatalogID != want {
			t.Errorf("%s catalogId = %q, want %q -- the identity template did not resolve",
				slug, catalog.CatalogID, want)
		}
		// The file's `output.filenamePrefix` is not used for the filename --
		// metadata.name is -- so assert what actually reaches disk.
		path := filepath.Join(report.OutDir, "example-"+slug+".json")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("catalog for %s was not written: %v", slug, err)
		}
	}

	// The chunk boundary, from the rendered document rather than the report:
	// the first chunk carries the budget and the second the remainder.
	if got := decodeFixtureCatalog(t, bySlug["AA"].Content); got.Count != 2 {
		t.Errorf("AA carries %v records, want 2 (the chunk budget)", got.Count)
	}
	if got := decodeFixtureCatalog(t, bySlug["AA-2"].Content); got.Count != 1 {
		t.Errorf("AA-2 carries %v records, want 1 (the remainder)", got.Count)
	}

	if report.Published != nil {
		t.Error("a run with Publish unset reported a publish result")
	}
	if len(runLog.recorded) != 1 {
		t.Errorf("recorded %d runs, want 1", len(runLog.recorded))
	}
}

// The corruption this design exists to prevent: two DIFFERENT pipelines
// pointed at one configured directory must not delete or publish each other's
// catalogs.
//
// The stale sweep runs before every collection and deletes by filename prefix,
// and the publish step globs by the same prefix -- so if both pipelines wrote
// into the shared directory, the second run would erase the first's and then
// post whatever was left as its own. It would look healthy on both sides.
func TestRunKeepsEachPipelineInItsOwnDirectory(t *testing.T) {
	upstream := newFixtureUpstream(t, twoGroupsOfThings)
	shared := t.TempDir()
	now := firedAt(t)

	run := func(files Files) RunReport {
		t.Helper()
		record := publishingRecord()
		record.Actions["publish"] = model.ActionPlan{Mappings: files.URL}

		report, err := Run(context.Background(), RunOptions{
			Pipeline: files,
			Record:   record,
			Lookup:   fixtureEnv(upstream.URL),
			Now:      now,
			OutDir:   shared,
		})
		if err != nil {
			t.Fatalf("Run(%s): %v", files.URL, err)
		}
		return report
	}

	first := run(fixturePipeline())
	second := run(otherPipeline())

	if first.OutDir == second.OutDir {
		t.Fatalf("both pipelines wrote to %s; each would sweep away the other's catalogs", first.OutDir)
	}
	if filepath.Dir(first.OutDir) != shared || filepath.Dir(second.OutDir) != shared {
		t.Errorf("directories %q and %q are not both beneath %q", first.OutDir, second.OutDir, shared)
	}

	// The first pipeline's catalogs must have survived the second's run --
	// this is what fails if the sweep is pointed at a shared directory.
	if _, err := os.Stat(filepath.Join(first.OutDir, "example-AA.json")); err != nil {
		t.Errorf("the first pipeline's catalog did not survive the second's run: %v", err)
	}
}

// A step that fails must fail the run, and the run must not be recorded: a
// transient upstream outage at midnight is retried on the next tick rather
// than costing the whole day.
func TestRunDoesNotRecordAFailedRun(t *testing.T) {
	runLog := &fakeRunLog{}

	_, err := Run(context.Background(), RunOptions{
		Pipeline: fixturePipeline(),
		Record:   publishingRecord(),
		RunLog:   runLog,
		Lookup:   fixtureEnv("http://127.0.0.1:1"), // nothing listens
		Now:      firedAt(t),
		OutDir:   t.TempDir(),
	})
	if err == nil {
		t.Fatal("a run against an unreachable upstream reported success")
	}
	if len(runLog.recorded) != 0 {
		t.Error("a failed run was recorded; the day's retry is now lost")
	}
}

// failWhenEmpty is the file's own refusal: an empty result walks cleanly
// through every later step and produces a well-formed, zero-error, empty run.
func TestRunHonoursTheFilesFailWhenEmpty(t *testing.T) {
	upstream := newFixtureUpstream(t, `[]`)

	_, err := Run(context.Background(), RunOptions{
		Pipeline: fixturePipeline(),
		Record:   publishingRecord(),
		Lookup:   fixtureEnv(upstream.URL),
		Now:      firedAt(t),
		OutDir:   t.TempDir(),
	})
	if err == nil {
		t.Fatal("an empty upstream produced a successful, empty run")
	}
}

func TestRunRefusesWithoutAPipeline(t *testing.T) {
	if _, err := Run(context.Background(), RunOptions{Record: publishingRecord()}); err == nil {
		t.Fatal("a run with no pipeline was attempted")
	}
}

// The registry may sanction a path that is not this pipeline's. Running the
// embedded one anyway would publish under a capability nobody authorised.
func TestRunRefusesAPipelineTheRegistryDidNotName(t *testing.T) {
	upstream := newFixtureUpstream(t, twoGroupsOfThings)

	record := publishingRecord()
	record.Actions["publish"] = model.ActionPlan{
		Mappings: "pkg/plugin/implementation/SomethingElse/its-own-pipeline.yaml",
	}

	_, err := Run(context.Background(), RunOptions{
		Pipeline: fixturePipeline(),
		Record:   record,
		Lookup:   fixtureEnv(upstream.URL),
		Now:      firedAt(t),
	})
	if err == nil {
		t.Fatal("a pipeline the registry did not name was run")
	}
	assertNeverCalled(t, upstream)
}

// RunOptions.PublishURL is the crawler's one publish address, and it wins over
// whatever the pipeline's own publishUrl input resolves to: every pipeline a
// crawler runs publishes where the crawler does.
func TestRunPublishesToTheCallersPublishURL(t *testing.T) {
	upstream := newFixtureUpstream(t, twoGroupsOfThings)
	pub := publisherAnswering(StatusPublished)

	// The fixture env points publishUrl at http://publish.invalid; the
	// caller's address must be the one used.
	report, err := Run(context.Background(), RunOptions{
		Pipeline:   fixturePipeline(),
		Record:     publishingRecord(),
		Lookup:     fixtureEnv(upstream.URL),
		Now:        firedAt(t),
		OutDir:     t.TempDir(),
		Publish:    true,
		PublishURL: "http://caller.test",
		Publisher:  pub,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if pub.calls() != len(report.Catalogs) || pub.calls() == 0 {
		t.Errorf("the publisher got %d bodies, want one per catalog (%d)", pub.calls(), len(report.Catalogs))
	}
	for _, url := range pub.urls {
		if url != "http://caller.test" {
			t.Errorf("published to %q, want the caller's address", url)
		}
	}
}

// The run's own clock, not the wall clock, decides what "today" is. Without
// this a golden file pins whatever day it was generated on and fails the next
// morning -- which is exactly what happened.
func TestExecuteResolvesDatesFromTheRunClock(t *testing.T) {
	spec := Spec{
		Schedule: Schedule{Cron: "0 0 * * *", UTCOffset: "+05:30"},
		Inputs: map[string]Input{
			"fromDate": {Type: "date", Format: "dd-MM-yyyy", Default: "today"},
		},
	}
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	now := time.Date(2026, 9, 21, 6, 0, 0, 0, ist)

	resolved, err := resolveRunInputs(spec, func(string) (string, bool) { return "", false }, RunOptions{}, now)
	if err != nil {
		t.Fatalf("resolveRunInputs: %v", err)
	}
	if got := resolved["fromDate"]; got != "21-09-2026" {
		t.Fatalf("fromDate = %q, want 21-09-2026 (the run's clock, not the wall clock)", got)
	}
}

// The caller's publish address wins over the pipeline's own input.
func TestResolveRunInputsAppliesThePublishURL(t *testing.T) {
	spec := Spec{
		Schedule: Schedule{UTCOffset: "+05:30"},
		Inputs:   map[string]Input{"publishUrl": {Env: "X_PUBLISH_URL", Default: "http://file.invalid"}},
	}
	resolved, err := resolveRunInputs(spec, func(string) (string, bool) { return "", false },
		RunOptions{PublishURL: " http://crawler.invalid "}, time.Now())
	if err != nil {
		t.Fatalf("resolveRunInputs: %v", err)
	}
	if got := resolved["publishUrl"]; got != "http://crawler.invalid" {
		t.Fatalf("publishUrl = %q, want the caller's", got)
	}
}

// claimingRunLog is a run log keyed by pipeline, with the claim a real store
// makes. Rows are per key, so a test can prove which key a run used.
type claimingRunLog struct {
	last     map[string]time.Time
	refuse   bool // another replica already holds the firing
	claimed  []string
	firings  []time.Time
	released map[string]time.Time
	recorded map[string]time.Time
}

func newClaimingRunLog() *claimingRunLog {
	return &claimingRunLog{last: map[string]time.Time{}, released: map[string]time.Time{},
		recorded: map[string]time.Time{}}
}

func (f *claimingRunLog) LastPipelineRun(_ context.Context, key string) (time.Time, error) {
	return f.last[key], nil
}

func (f *claimingRunLog) RecordPipelineRun(_ context.Context, key string, at time.Time) error {
	f.recorded[key] = at
	f.last[key] = at
	return nil
}

func (f *claimingRunLog) ClaimPipelineRun(_ context.Context, key string, now, firing time.Time) (bool, error) {
	f.firings = append(f.firings, firing)
	if f.refuse {
		return false, nil
	}
	f.claimed = append(f.claimed, key)
	f.last[key] = now
	return true, nil
}

func (f *claimingRunLog) ReleasePipelineRun(ctx context.Context, key string, previous time.Time) error {
	// A real store's release is a database write: on an already-cancelled or
	// timed-out context it fails immediately, the same as any other query.
	if err := ctx.Err(); err != nil {
		return err
	}
	f.released[key] = previous
	f.last[key] = previous
	return nil
}

// The run log is keyed on the PIPELINE, not the capability: two sources for
// one capability must not share a row and starve each other.
func TestRunKeysTheRunLogOnThePipeline(t *testing.T) {
	upstream := newFixtureUpstream(t, twoGroupsOfThings)
	runLog := newClaimingRunLog()

	if _, err := Run(context.Background(), RunOptions{
		Pipeline: fixturePipeline(), Record: publishingRecord(), RunLog: runLog,
		Lookup: fixtureEnv(upstream.URL), Now: firedAt(t), OutDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	key := fixturePipeline().URL
	if len(runLog.claimed) != 1 || runLog.claimed[0] != key {
		t.Errorf("claimed %v, want exactly [%s]", runLog.claimed, key)
	}
	if _, ok := runLog.recorded[key]; !ok {
		t.Errorf("recorded %v, want a row under the pipeline %s", runLog.recorded, key)
	}
	// The claim's window is the firing the schedule says is due -- midnight
	// in the pipeline's own zone -- not the run's wall clock.
	ist, _ := time.LoadLocation("Asia/Kolkata")
	if want := time.Date(2026, 9, 21, 0, 0, 0, 0, ist); len(runLog.firings) != 1 || !runLog.firings[0].Equal(want) {
		t.Errorf("claimed with firing %v, want %v", runLog.firings, want)
	}
	if _, ok := runLog.recorded[fixtureCapability]; ok {
		t.Errorf("a row was written under the capability %s", fixtureCapability)
	}
}

// A run whose context is cancelled MID-FLIGHT -- after it already won the
// claim -- still must give that claim back. Otherwise the claim marker stays
// set to "now", and the firing looks already handled for the rest of the
// schedule window: dueNow would not fire it again until the NEXT scheduled
// time, silently losing this one to what was only a shutdown.
func TestRunReleasesTheClaimEvenWhenItsContextWasCancelledMidFlight(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // held open until the test cancels ctx
	}))
	t.Cleanup(server.Close)

	runLog := newClaimingRunLog()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
		close(release)
	}()

	if _, err := Run(ctx, RunOptions{
		Pipeline: fixturePipeline(), Record: publishingRecord(), RunLog: runLog,
		Lookup: fixtureEnv(server.URL), Now: firedAt(t), OutDir: t.TempDir(),
	}); err == nil {
		t.Fatal("Run succeeded despite its context being cancelled mid-flight")
	}

	key := fixturePipeline().URL
	if len(runLog.claimed) != 1 {
		t.Fatalf("claimed %v, want the claim to have been won before the cancellation landed", runLog.claimed)
	}
	if _, released := runLog.released[key]; !released {
		t.Error("the claim was not released after a mid-flight cancellation")
	}
}

// A cancelled run must leave the last good catalogs on disk. Clearing them
// before the steps run means a shutdown mid-fetch deletes yesterday's output
// and writes nothing in its place.
func TestACancelledRunKeepsTheLastGoodCatalogs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The token exchange succeeds; the cancel lands on the data call, after
	// the run has got past credentials and into its steps.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"token":"tok-fixture"}`))
			return
		}
		cancel()
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)

	outDir := t.TempDir()
	previous := filepath.Join(outDir, "example-Thing", "example-AA.json")
	if err := os.MkdirAll(filepath.Dir(previous), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(previous, []byte(`{"from":"the last good run"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Run(ctx, RunOptions{
		Pipeline: fixturePipeline(), Record: publishingRecord(), RunLog: &fakeRunLog{},
		Lookup: fixtureEnv(server.URL), Now: firedAt(t), OutDir: outDir,
	}); err == nil {
		t.Fatal("Run succeeded despite its context being cancelled mid-flight")
	}

	if _, err := os.Stat(previous); err != nil {
		t.Errorf("the last good catalog is gone after a cancelled run: %v", err)
	}
}

// Another replica already claimed this firing: this one stands down before
// fetching anything, and says why.
func TestRunStandsDownWhenAnotherReplicaClaimedTheFiring(t *testing.T) {
	upstream := newFixtureUpstream(t, twoGroupsOfThings)
	runLog := newClaimingRunLog()
	runLog.refuse = true

	report, err := Run(context.Background(), RunOptions{
		Pipeline: fixturePipeline(), Record: publishingRecord(), RunLog: runLog,
		Lookup: fixtureEnv(upstream.URL), Now: firedAt(t), OutDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.ClaimedElsewhere || !strings.Contains(report.Reason, "another replica") {
		t.Errorf("report = %+v; want ClaimedElsewhere and a reason naming the other replica", report)
	}
	if len(report.Catalogs) != 0 || upstream.calls.Load() != 0 {
		t.Errorf("built %d catalogs and made %d upstream calls; a stood-down run does no work",
			len(report.Catalogs), upstream.calls.Load())
	}
	if len(runLog.recorded) != 0 {
		t.Errorf("a stood-down run recorded %v", runLog.recorded)
	}
}

// A failed run gives its claim back, restoring the previous marker, so the
// next tick retries the firing rather than treating it as served.
func TestRunReleasesTheClaimWhenTheRunFails(t *testing.T) {
	runLog := newClaimingRunLog()
	key := fixturePipeline().URL
	previous := firedAt(t).Add(-24 * time.Hour)
	runLog.last[key] = previous

	if _, err := Run(context.Background(), RunOptions{
		Pipeline: fixturePipeline(), Record: publishingRecord(), RunLog: runLog,
		Lookup: fixtureEnv("http://127.0.0.1:1"), Now: firedAt(t), OutDir: t.TempDir(),
	}); err == nil {
		t.Fatal("a run against an unreachable upstream reported success")
	}
	if got, ok := runLog.released[key]; !ok || !got.Equal(previous) {
		t.Errorf("released %v, want %s restored to %v", runLog.released, key, previous)
	}
	if len(runLog.recorded) != 0 {
		t.Errorf("a failed run was recorded: %v", runLog.recorded)
	}
}

// LEGACY: before this change the row was keyed by capability. The first tick
// after deploy must read it, or a firing already served is run -- and
// published -- a second time.
func TestRunReadsTheLegacyCapabilityKeyedRow(t *testing.T) {
	upstream := newFixtureUpstream(t, twoGroupsOfThings)
	runLog := newClaimingRunLog()
	runLog.last[fixtureCapability] = firedAt(t) // served under the old key

	report, err := Run(context.Background(), RunOptions{
		Pipeline: fixturePipeline(), Record: publishingRecord(), RunLog: runLog,
		Lookup: fixtureEnv(upstream.URL), Now: firedAt(t), OutDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Due {
		t.Errorf("report = %+v; the legacy row says this firing was served", report)
	}
	if len(runLog.claimed) != 0 || upstream.calls.Load() != 0 {
		t.Errorf("claimed %v and made %d calls for a served firing", runLog.claimed, upstream.calls.Load())
	}
}

// An input the file marks required must resolve to something, or the run is
// refused naming it and where it comes from. participantId is the case: with
// no default, an unset variable would otherwise publish catalogs under an
// empty provider id.
func TestResolveRunInputsRefusesAnEmptyRequiredInput(t *testing.T) {
	spec := Spec{
		Schedule: Schedule{UTCOffset: "+05:30"},
		Inputs: map[string]Input{
			"participantId": {Env: "MANDI_PARTICIPANT_ID", Required: true},
			"optional":      {Env: "X_OPTIONAL"},
		},
	}
	_, err := resolveRunInputs(spec, func(string) (string, bool) { return "", false }, RunOptions{}, time.Now())
	if err == nil {
		t.Fatal("an empty required input was accepted")
	}
	for _, want := range []string{"participantId", "MANDI_PARTICIPANT_ID"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}

	resolved, err := resolveRunInputs(spec, func(name string) (string, bool) {
		return map[string]string{"MANDI_PARTICIPANT_ID": "bpp.example"}[name], name == "MANDI_PARTICIPANT_ID"
	}, RunOptions{}, time.Now())
	if err != nil || resolved["participantId"] != "bpp.example" {
		t.Fatalf("a set required input: %v, %v", resolved, err)
	}
}

// ${schedule.utcOffset} is the schedule zone's offset at the run's clock, so a
// timestamp built from a date input says which day it means.
func TestScheduleUTCOffsetIsTheZonesOffset(t *testing.T) {
	rc := newRunContext(nil, "")
	if _, err := rc.lookup("schedule.utcOffset"); err == nil {
		t.Fatal("${schedule.utcOffset} resolved before the run set it")
	}
	rc.utcOffset = offsetIn("+05:30", time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC))
	got, err := rc.with("state", map[string]any{}).lookup("schedule.utcOffset")
	if err != nil || got != "+05:30" {
		t.Fatalf("${schedule.utcOffset} = %v, %v; want +05:30 (and carried into a loop scope)", got, err)
	}
}

// overrideSpec is a pipeline named "x" with one ordinary input, one date input
// and one credential.
func overrideSpec() Spec {
	return Spec{
		Metadata: Metadata{Name: "x"},
		Schedule: Schedule{Cron: "0 0 * * *", UTCOffset: "+05:30"},
		Inputs: map[string]Input{
			"participantId": {Env: "X_PARTICIPANT_ID", Default: "from-file"},
			"fromDate":      {Type: "date", Format: "dd-MM-yyyy", Default: "today"},
			"tokenSecret":   {Env: "X_TOKEN_SECRET", Secret: true},
		},
	}
}

// Plugin config wins over env, which wins over the file: an operator changes
// a pipeline's identity or window in the adapter's own config, without a
// rebuild -- and an overridden input is still held to its declared type.
func TestPluginConfigOverridesAPipelineInput(t *testing.T) {
	env := func(name string) (string, bool) {
		return map[string]string{"X_PARTICIPANT_ID": "from-env"}[name], name == "X_PARTICIPANT_ID"
	}
	ist, _ := time.LoadLocation("Asia/Kolkata")
	resolved, err := resolveRunInputs(overrideSpec(), env, RunOptions{Config: map[string]string{
		"publish.x.participantId": "from-config",
		"publish.x.fromDate":      "yesterday",
		"publish.other.fromDate":  "01-01-2020", // another pipeline's: not this one's business
		"discoveryPushUrl":        "https://ignored",
	}}, time.Date(2026, 9, 21, 6, 0, 0, 0, ist))
	if err != nil {
		t.Fatalf("resolveRunInputs: %v", err)
	}
	if resolved["participantId"] != "from-config" || resolved["fromDate"] != "20-09-2026" {
		t.Fatalf("resolved = %v; want participantId from config and fromDate typed as yesterday", resolved)
	}
}

// A typo in an override key is refused, naming what exists; a credential may
// not come from plugin config at all -- config files are read, logged and
// committed, secrets are not.
func TestPluginConfigOverridesAreChecked(t *testing.T) {
	cases := map[string]struct {
		key    string
		expect []string
	}{
		"an unknown input": {key: "publish.x.participantid", expect: []string{"participantid", "participantId"}},
		"a credential":     {key: "publish.x.tokenSecret", expect: []string{"tokenSecret", "secret"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := resolveRunInputs(overrideSpec(), func(string) (string, bool) { return "", false },
				RunOptions{Config: map[string]string{tc.key: "v"}}, time.Now())
			if err == nil {
				t.Fatalf("override %s was accepted", tc.key)
			}
			for _, want := range tc.expect {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
		})
	}
}

// publish.<name>.schedule replaces the file's cron, so a schedule change is a
// config edit. The file says midnight; config says 03:00, and at 04:00 after a
// 00:01 run only the override makes the pipeline due again.
func TestPluginConfigOverridesTheSchedule(t *testing.T) {
	ist, _ := time.LoadLocation("Asia/Kolkata")
	runLog := &fakeRunLog{last: time.Date(2026, 9, 21, 0, 1, 0, 0, ist)}
	opts := RunOptions{
		Pipeline: fixturePipeline(), Record: publishingRecord(), RunLog: runLog,
		Lookup: unreachableEnv, Now: time.Date(2026, 9, 21, 4, 0, 0, 0, ist), DryRun: true,
	}
	report, err := Run(context.Background(), opts)
	if err != nil || report.Due {
		t.Fatalf("without the override: due=%v err=%v; the file's midnight firing was served", report.Due, err)
	}

	opts.Config = map[string]string{"publish.example.schedule": "0 3 * * *"}
	report, err = Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Due || !strings.Contains(report.Reason, "0 3 * * *") {
		t.Errorf("report = due %v, %q; want due under the configured 0 3 * * *", report.Due, report.Reason)
	}

	opts.Config = map[string]string{"publish.example.schedule": "every day"}
	if _, err := Run(context.Background(), opts); err == nil {
		t.Error("an unparseable configured schedule was accepted")
	}
}

// A mistyped override fails every tick, not only the tick the pipeline is
// due -- otherwise the typo sits unnoticed until midnight.
func TestAMistypedOverrideFailsBeforeTheDueCheck(t *testing.T) {
	ist, _ := time.LoadLocation("Asia/Kolkata")
	_, err := Run(context.Background(), RunOptions{
		Pipeline: fixturePipeline(), Record: publishingRecord(),
		RunLog: &fakeRunLog{last: time.Date(2026, 9, 21, 0, 1, 0, 0, ist)}, // served: not due
		Lookup: unreachableEnv, Now: time.Date(2026, 9, 21, 4, 0, 0, 0, ist), DryRun: true,
		Config: map[string]string{"publish.example.publishURL": "https://x"}, // it is publishUrl
	})
	if err == nil || !strings.Contains(err.Error(), "publishURL") {
		t.Fatalf("err = %v; want the mistyped key refused even though the pipeline is not due", err)
	}
}

// Config faults found before the due check recur on every tick. They are
// permanent, so the sweep says them once instead of spending its budget on
// them and marking the firing served every third tick.
func TestConfigFaultsBeforeTheDueCheckArePermanent(t *testing.T) {
	ist, _ := time.LoadLocation("Asia/Kolkata")
	for name, config := range map[string]map[string]string{
		"a mistyped override":  {"publish.example.publishURL": "https://x"},
		"an unusable schedule": {"publish.example.schedule": "every day"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Run(context.Background(), RunOptions{
				Pipeline: fixturePipeline(), Record: publishingRecord(), RunLog: &fakeRunLog{},
				Lookup: unreachableEnv, Now: time.Date(2026, 9, 21, 4, 0, 0, 0, ist), DryRun: true,
				Config: config,
			})
			if err == nil || !crawler.IsPermanent(err) {
				t.Errorf("err = %v, want a permanent fault", err)
			}
		})
	}
}

// ---- tests for the catalog-building code (formerly catalog_test.go) ----

// echoMapper stands in for jsonmapper: it hands back whatever the builder
// passed it, so a test can assert on the INPUT the render mapping would see
// without a mapping file existing.
type echoMapper struct {
	calls int
	refs  []string
}

func (m *echoMapper) Verify(context.Context, string, any) error { return nil }

func (m *echoMapper) Transform(_ context.Context, ref string, d definition.Direction, in any) ([]byte, error) {
	m.calls++
	m.refs = append(m.refs, ref)
	return json.Marshal(map[string]any{"direction": string(d), "input": in})
}

// failingMapper reports what a broken mapping does, so a render failure is
// distinguishable from a build failure.
type failingMapper struct{}

func (failingMapper) Verify(context.Context, string, any) error { return nil }

func (failingMapper) Transform(context.Context, string, definition.Direction, any) ([]byte, error) {
	return nil, fmt.Errorf("mapping blew up")
}

func testBuildContext(t *testing.T, inputs map[string]string) (*runContext, *exprCache) {
	t.Helper()
	cache, err := newExprCache()
	if err != nil {
		t.Fatalf("newExprCache: %v", err)
	}
	if inputs == nil {
		inputs = map[string]string{}
	}
	return newRunContext(inputs, "never-logged-token"), cache
}

// decodeEcho pulls the render input back out of what echoMapper returned.
func decodeEcho(t *testing.T, content []byte) (response []any, local map[string]any) {
	t.Helper()
	var echoed struct {
		Direction string `json:"direction"`
		Input     struct {
			Response []any          `json:"response"`
			Local    map[string]any `json:"_local"`
		} `json:"input"`
	}
	if err := json.Unmarshal(content, &echoed); err != nil {
		t.Fatalf("decoding rendered content: %v", err)
	}
	if echoed.Direction != string(definition.DirectionResponse) {
		t.Errorf("render ran in direction %q, want %q", echoed.Direction, definition.DirectionResponse)
	}
	return echoed.Input.Response, echoed.Input.Local
}

// zeroCommoditiesRule is "this record lists nothing", and it is NOT spelled
// `$count(commodities) = 0` the way agmarknet.yaml spells it.
//
// Under this jsonata build that comparison is ALWAYS false: a field holding an
// empty array resolves to an empty sequence, and the 0 that $count returns for
// one does not compare equal to the literal 0 (`$count(x) < 1` over the same
// record fails outright with "the values 0 and 1 ... must be of the same data
// type"). $number() puts it back into the number the comparison expects.
//
// The builder is right either way -- it applies what the file says -- but the
// file's own rule silently excludes nothing, so no test here pretends
// otherwise.
const zeroCommoditiesRule = "$number($count(commodities)) = 0"

// simpleCatalog is the smallest catalog block that builds anything: group,
// name, render.
func simpleCatalog() Catalog {
	return Catalog{
		GroupBy:  "stateCode",
		Chunk:    Chunk{Slug: "${stateCode}"},
		Identity: Identity{CatalogID: "catalog:test:${slug}"},
		Render:   Render{Mapping: "mappings/catalog.yaml"},
	}
}

func TestBuildCatalogsGroupsByTheNamedField(t *testing.T) {
	rc, cache := testBuildContext(t, nil)
	mapper := &echoMapper{}

	records := []map[string]any{
		{"stateCode": "MH", "marketId": 2.0},
		{"stateCode": "KA", "marketId": 1.0},
		{"stateCode": "MH", "marketId": 3.0},
	}

	built, counters, err := buildCatalogs(context.Background(), simpleCatalog(), records, rc, cache, mapper, "http://mappings/pipeline.yaml")
	if err != nil {
		t.Fatalf("buildCatalogs: %v", err)
	}
	if len(built) != 2 {
		t.Fatalf("built %d catalogs, want 2", len(built))
	}
	// Groups are walked in sorted key order so two runs read the same way.
	if built[0].Slug != "KA" || built[1].Slug != "MH" {
		t.Errorf("slugs are %q/%q, want KA/MH", built[0].Slug, built[1].Slug)
	}
	if built[1].CatalogID != "catalog:test:MH" {
		t.Errorf("catalogId is %q", built[1].CatalogID)
	}
	if counters["groups"] != 2 || counters["catalogs"] != 2 {
		t.Errorf("counters = %v", counters)
	}

	response, _ := decodeEcho(t, built[1].Content)
	if len(response) != 2 {
		t.Fatalf("MH carries %d records, want 2", len(response))
	}
	if mapper.refs[0] != "http://mappings/mappings/catalog.yaml" {
		t.Errorf("mapping ref is %q, want it resolved beside the pipeline file", mapper.refs[0])
	}
}

func TestBuildCatalogsRefusesARecordMissingTheGroupField(t *testing.T) {
	rc, cache := testBuildContext(t, nil)

	records := []map[string]any{{"marketId": 1.0}}
	if _, _, err := buildCatalogs(context.Background(), simpleCatalog(), records, rc, cache, &echoMapper{}, "http://mappings/pipeline.yaml"); err == nil {
		t.Fatal("a record with nothing to group by built a catalog")
	}
}

func TestBuildCatalogsExclusions(t *testing.T) {
	commodityRule := ExcludeRule{
		// NOT `$count(commodities) = 0`: see the note on
		// zeroCommoditiesRule below.
		When:   zeroCommoditiesRule,
		Reason: "upstream reported no commodities trading in this window",
	}
	geometryRule := ExcludeRule{
		When:   "${inputs.withoutGeometry} = 'skip' and coordinateQuality != 'ok'",
		Reason: "coordinate ${coordinateQuality}",
	}

	records := []map[string]any{
		{"stateCode": "MH", "marketId": 1.0, "commodities": []any{"wheat"}, "coordinateQuality": "ok"},
		{"stateCode": "MH", "marketId": 2.0, "commodities": []any{}, "coordinateQuality": "ok"},
		{"stateCode": "MH", "marketId": 3.0, "commodities": []any{"rice"}, "coordinateQuality": "missing"},
	}

	cases := []struct {
		name            string
		withoutGeometry string
		wantPublished   int
		wantCounters    map[string]int
	}{
		{
			// The geometry rule reads an input. Interpolated as bare text it
			// would read `publish = 'skip'` -- a comparison against a PATH
			// called publish, which is undefined, so the rule would never fire
			// either way and the skip option would silently do nothing.
			name:            "publish keeps the geometry-less market",
			withoutGeometry: "publish",
			wantPublished:   2,
			wantCounters: map[string]int{
				"excluded": 1,
				"excluded:upstream reported no commodities trading in this window": 1,
			},
		},
		{
			name:            "skip drops it, naming the verdict",
			withoutGeometry: "skip",
			wantPublished:   1,
			wantCounters: map[string]int{
				"excluded":                    2,
				"excluded:coordinate missing": 1,
				"excluded:upstream reported no commodities trading in this window": 1,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc, cache := testBuildContext(t, map[string]string{"withoutGeometry": tc.withoutGeometry})
			catalog := simpleCatalog()
			catalog.Exclude = []ExcludeRule{commodityRule, geometryRule}

			built, counters, err := buildCatalogs(context.Background(), catalog, records, rc, cache, &echoMapper{}, "http://mappings/pipeline.yaml")
			if err != nil {
				t.Fatalf("buildCatalogs: %v", err)
			}
			if len(built) != 1 {
				t.Fatalf("built %d catalogs, want 1", len(built))
			}
			response, _ := decodeEcho(t, built[0].Content)
			if len(response) != tc.wantPublished {
				t.Fatalf("catalog carries %d records, want %d", len(response), tc.wantPublished)
			}
			for key, want := range tc.wantCounters {
				if counters[key] != want {
					t.Errorf("counter %q = %d, want %d (all: %v)", key, counters[key], want, counters)
				}
			}
		})
	}
}

func TestBuildCatalogsAnnotatesWithoutExcluding(t *testing.T) {
	rc, cache := testBuildContext(t, nil)
	catalog := simpleCatalog()
	catalog.Annotate = []AnnotateRule{{When: "coordinateQuality != 'ok'", As: "publishedWithoutLocation"}}

	records := []map[string]any{
		{"stateCode": "MH", "marketId": 1.0, "coordinateQuality": "ok"},
		{"stateCode": "MH", "marketId": 2.0, "coordinateQuality": "suspect"},
	}

	built, counters, err := buildCatalogs(context.Background(), catalog, records, rc, cache, &echoMapper{}, "http://mappings/pipeline.yaml")
	if err != nil {
		t.Fatalf("buildCatalogs: %v", err)
	}
	response, _ := decodeEcho(t, built[0].Content)
	if len(response) != 2 {
		t.Fatalf("an annotated record was dropped: %d records", len(response))
	}
	first := response[0].(map[string]any)
	second := response[1].(map[string]any)
	if _, marked := first["publishedWithoutLocation"]; marked {
		t.Error("the market with an ok coordinate was annotated")
	}
	if second["publishedWithoutLocation"] != true {
		t.Errorf("the geometry-less market was not annotated: %v", second)
	}
	if counters["annotated:publishedWithoutLocation"] != 1 {
		t.Errorf("counters = %v", counters)
	}
	// The caller's records must not have grown a field behind its back.
	if _, leaked := records[1]["publishedWithoutLocation"]; leaked {
		t.Error("annotation wrote onto the caller's record")
	}
}

func TestBuildCatalogsOrders(t *testing.T) {
	records := []map[string]any{
		{"stateCode": "MH", "marketId": 10.0},
		{"stateCode": "MH", "marketId": 9.0},
		{"stateCode": "MH", "marketId": 100.0},
	}

	cases := []struct {
		name      string
		direction string
		want      []float64
	}{
		// 9 before 10 before 100: numeric keys must not order as text.
		{name: "asc", direction: "asc", want: []float64{9, 10, 100}},
		{name: "desc", direction: "desc", want: []float64{100, 10, 9}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc, cache := testBuildContext(t, nil)
			catalog := simpleCatalog()
			catalog.Order = Order{By: "marketId", Direction: tc.direction}

			built, _, err := buildCatalogs(context.Background(), catalog, records, rc, cache, &echoMapper{}, "http://mappings/pipeline.yaml")
			if err != nil {
				t.Fatalf("buildCatalogs: %v", err)
			}
			response, _ := decodeEcho(t, built[0].Content)
			for i, want := range tc.want {
				if got := response[i].(map[string]any)["marketId"]; got != want {
					t.Errorf("position %d is %v, want %v", i, got, want)
				}
			}
		})
	}
}

func TestBuildCatalogsRefusesAnUnknownOrderDirection(t *testing.T) {
	rc, cache := testBuildContext(t, nil)
	catalog := simpleCatalog()
	catalog.Order = Order{By: "marketId", Direction: "sideways"}

	records := []map[string]any{{"stateCode": "MH", "marketId": 1.0}}
	if _, _, err := buildCatalogs(context.Background(), catalog, records, rc, cache, &echoMapper{}, "http://mappings/pipeline.yaml"); err == nil {
		t.Fatal("an unknown order direction was accepted")
	}
}

// TestChunkAtTheBudgetBoundary is the rule the geometry cap turns on: a chunk
// is cut only when adding the NEXT record would exceed the budget, so exactly
// budget-many costing records still make one catalog.
func TestChunkAtTheBudgetBoundary(t *testing.T) {
	chunk := Chunk{
		Budget: 4,
		Cost:   "coordinateQuality = 'ok' ? 1 : 0",
		Slug:   "${stateCode}${chunkIndex > 1 ? '-' & chunkIndex : ''}",
	}

	costing := func(n int) []map[string]any {
		records := make([]map[string]any, 0, n)
		for i := 0; i < n; i++ {
			records = append(records, map[string]any{
				"stateCode": "MH", "marketId": float64(i), "coordinateQuality": "ok",
			})
		}
		return records
	}

	cases := []struct {
		name      string
		records   []map[string]any
		wantSlugs []string
		wantSizes []int
	}{
		{name: "exactly the budget", records: costing(4), wantSlugs: []string{"MH"}, wantSizes: []int{4}},
		{name: "one over the budget", records: costing(5), wantSlugs: []string{"MH", "MH-2"}, wantSizes: []int{4, 1}},
		{name: "two full chunks", records: costing(8), wantSlugs: []string{"MH", "MH-2"}, wantSizes: []int{4, 4}},
		{
			// Free records never force a split, however many there are.
			name: "costless records all fit",
			records: []map[string]any{
				{"stateCode": "MH", "marketId": 1.0, "coordinateQuality": "missing"},
				{"stateCode": "MH", "marketId": 2.0, "coordinateQuality": "missing"},
				{"stateCode": "MH", "marketId": 3.0, "coordinateQuality": "missing"},
				{"stateCode": "MH", "marketId": 4.0, "coordinateQuality": "missing"},
				{"stateCode": "MH", "marketId": 5.0, "coordinateQuality": "missing"},
			},
			wantSlugs: []string{"MH"},
			wantSizes: []int{5},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc, cache := testBuildContext(t, nil)
			catalog := simpleCatalog()
			catalog.Chunk = chunk
			catalog.Order = Order{By: "marketId", Direction: "asc"}

			built, _, err := buildCatalogs(context.Background(), catalog, tc.records, rc, cache, &echoMapper{}, "http://mappings/pipeline.yaml")
			if err != nil {
				t.Fatalf("buildCatalogs: %v", err)
			}
			if len(built) != len(tc.wantSlugs) {
				t.Fatalf("built %d catalogs, want %d", len(built), len(tc.wantSlugs))
			}
			for i, want := range tc.wantSlugs {
				if built[i].Slug != want {
					t.Errorf("chunk %d slug is %q, want %q", i, built[i].Slug, want)
				}
				if built[i].CatalogID != "catalog:test:"+want {
					t.Errorf("chunk %d catalogId is %q", i, built[i].CatalogID)
				}
				response, _ := decodeEcho(t, built[i].Content)
				if len(response) != tc.wantSizes[i] {
					t.Errorf("chunk %d carries %d records, want %d", i, len(response), tc.wantSizes[i])
				}
			}
		})
	}
}

func TestChunkRefusesASlugThatCannotDistinguishChunks(t *testing.T) {
	rc, cache := testBuildContext(t, nil)
	catalog := simpleCatalog()
	catalog.Chunk = Chunk{Budget: 1, Cost: "1", Slug: "${stateCode}"}

	records := []map[string]any{
		{"stateCode": "MH", "marketId": 1.0},
		{"stateCode": "MH", "marketId": 2.0},
	}
	_, _, err := buildCatalogs(context.Background(), catalog, records, rc, cache, &echoMapper{}, "http://mappings/pipeline.yaml")
	if err == nil {
		t.Fatal("two chunks published under one slug")
	}
	if !strings.Contains(err.Error(), "slug") {
		t.Errorf("error does not name the slug: %v", err)
	}
}

func TestChunkRefusesANonNumericCost(t *testing.T) {
	rc, cache := testBuildContext(t, nil)
	catalog := simpleCatalog()
	catalog.Chunk = Chunk{Budget: 2, Cost: "'expensive'", Slug: "${stateCode}"}

	records := []map[string]any{{"stateCode": "MH", "marketId": 1.0}}
	if _, _, err := buildCatalogs(context.Background(), catalog, records, rc, cache, &echoMapper{}, "http://mappings/pipeline.yaml"); err == nil {
		t.Fatal("a cost that is not a number was accepted")
	}
}

// TestBuildCatalogsEmptyGroupProducesNoCatalog is the one rule that cannot
// be got wrong: an empty catalog retires the group's resources from the
// network on the next MERGE.
func TestBuildCatalogsEmptyGroupProducesNoCatalog(t *testing.T) {
	rc, cache := testBuildContext(t, nil)
	catalog := simpleCatalog()
	catalog.Exclude = []ExcludeRule{{When: zeroCommoditiesRule, Reason: "nothing traded"}}

	records := []map[string]any{
		{"stateCode": "MH", "marketId": 1.0, "commodities": []any{}},
		{"stateCode": "KA", "marketId": 2.0, "commodities": []any{"rice"}},
	}

	mapper := &echoMapper{}
	built, counters, err := buildCatalogs(context.Background(), catalog, records, rc, cache, mapper, "http://mappings/pipeline.yaml")
	if err != nil {
		t.Fatalf("buildCatalogs: %v", err)
	}
	if len(built) != 1 || built[0].Slug != "KA" {
		t.Fatalf("built %+v, want only KA", built)
	}
	if mapper.calls != 1 {
		t.Errorf("the render mapping ran %d times, want 1", mapper.calls)
	}
	if counters["excludedGroups"] != 1 {
		t.Errorf("an emptied group was not counted: %v", counters)
	}
}

func TestBuildCatalogsBrokenExpressionsAreErrorsNotFalse(t *testing.T) {
	records := []map[string]any{{"stateCode": "MH", "marketId": 1.0, "commodities": []any{"rice"}}}

	cases := []struct {
		name    string
		catalog func(Catalog) Catalog
	}{
		{
			name: "exclude",
			catalog: func(c Catalog) Catalog {
				c.Exclude = []ExcludeRule{{When: "$count(((", Reason: "broken"}}
				return c
			},
		},
		{
			name: "annotate",
			catalog: func(c Catalog) Catalog {
				c.Annotate = []AnnotateRule{{When: "$count(((", As: "broken"}}
				return c
			},
		},
		{
			name: "chunk cost",
			catalog: func(c Catalog) Catalog {
				c.Chunk = Chunk{Budget: 2, Cost: "$count(((", Slug: "${stateCode}"}
				return c
			},
		},
		{
			name: "an exclusion naming an input that was never declared",
			catalog: func(c Catalog) Catalog {
				c.Exclude = []ExcludeRule{{When: "${inputs.notDeclared} = 'skip'", Reason: "broken"}}
				return c
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc, cache := testBuildContext(t, nil)
			_, _, err := buildCatalogs(context.Background(), tc.catalog(simpleCatalog()), records, rc, cache, &echoMapper{}, "http://mappings/pipeline.yaml")
			if err == nil {
				t.Fatal("a broken expression was read as false instead of failing the build")
			}
		})
	}
}

func TestBuildCatalogsRendersLocalsAndIdentities(t *testing.T) {
	rc, cache := testBuildContext(t, map[string]string{
		"participantId": "agmarknet",
		"networkId":     "oan-dev",
	})
	catalog := simpleCatalog()
	catalog.Identity.ResourceID = "resource:test:market:${marketId}"
	catalog.Render.Local = map[string]string{
		"participantId": "${inputs.participantId}",
		"networkId":     "${inputs.networkId}",
		"catalogSlug":   "${slug}",
		"stateName":     "${group.stateName}",
	}

	records := []map[string]any{
		// The first row's stateName is blank, so a group value taken blindly
		// from the first record would name the catalog after nothing.
		{"stateCode": "MH", "marketId": 1.0, "stateName": ""},
		{"stateCode": "MH", "marketId": 2.0, "stateName": "Maharashtra"},
	}

	built, _, err := buildCatalogs(context.Background(), catalog, records, rc, cache, &echoMapper{}, "http://mappings/pipeline.yaml")
	if err != nil {
		t.Fatalf("buildCatalogs: %v", err)
	}
	response, local := decodeEcho(t, built[0].Content)
	want := map[string]any{
		"participantId": "agmarknet",
		"networkId":     "oan-dev",
		"catalogSlug":   "MH",
		"stateName":     "Maharashtra",
	}
	for key, value := range want {
		if local[key] != value {
			t.Errorf("_local[%q] = %v, want %v", key, local[key], value)
		}
	}
	if got := response[0].(map[string]any)["resourceId"]; got != "resource:test:market:1" {
		t.Errorf("resourceId is %v", got)
	}
}

func TestBuildCatalogsNeverCarriesTheToken(t *testing.T) {
	rc, cache := testBuildContext(t, nil)
	catalog := simpleCatalog()

	records := []map[string]any{{"stateCode": "MH", "marketId": 1.0}}
	built, _, err := buildCatalogs(context.Background(), catalog, records, rc, cache, &echoMapper{}, "http://mappings/pipeline.yaml")
	if err != nil {
		t.Fatalf("buildCatalogs: %v", err)
	}
	if strings.Contains(string(built[0].Content), "never-logged-token") {
		t.Error("the rendered catalog carries the upstream token")
	}
}

func TestBuildCatalogsReportsARenderFailure(t *testing.T) {
	rc, cache := testBuildContext(t, nil)
	records := []map[string]any{{"stateCode": "MH", "marketId": 1.0}}

	if _, _, err := buildCatalogs(context.Background(), simpleCatalog(), records, rc, cache, failingMapper{}, "http://mappings/pipeline.yaml"); err == nil {
		t.Fatal("a failing render mapping built a catalog")
	}
}

// TestBuildCatalogsFromADeclaredCatalogBlock runs the mandi file's OWN
// catalog block, parsed as YAML, over synthetic records: the point of this
// builder is that a capability adds one by writing YAML and no Go.
func TestBuildCatalogsFromADeclaredCatalogBlock(t *testing.T) {
	const block = `
groupBy: stateCode

exclude:
  - when: "$number($count(commodities)) = 0"
    reason: "upstream reported no commodities trading in this window"
  - when: "${inputs.withoutGeometry} = 'skip' and coordinateQuality != 'ok'"
    reason: "coordinate ${coordinateQuality}"

annotate:
  - when: "coordinateQuality != 'ok'"
    as: publishedWithoutLocation

order: { by: marketId, direction: asc }

chunk:
  budget: 2
  of: geometries
  cost: "coordinateQuality = 'ok' and $exists(latitude) and $exists(longitude) ? 1 : 0"
  slug: "${stateCode}${chunkIndex > 1 ? '-' & chunkIndex : ''}"

identity:
  catalogId: catalog:mandi-price:${slug}
  resourceId: resource:mandi-price:market:${marketId}

render:
  mapping: mappings/catalog.yaml
  local:
    participantId: ${inputs.participantId}
    networkId: ${inputs.networkId}
    catalogSlug: ${slug}
    stateName: ${group.stateName}
    windowFrom: ${inputs.fromDate}
    windowTo: ${inputs.toDate}
    generatedAt: ${now.rfc3339}
    transactionId: ${uuid}
    messageId: ${uuid}
    publishWithoutGeometry: ${inputs.withoutGeometry}

output:
  dir: ${inputs.catalogOut}
  file: mandi-${slug}.json
  filenamePrefix: mandi
`

	var catalog Catalog
	if err := yaml.Unmarshal([]byte(block), &catalog); err != nil {
		t.Fatalf("parsing the catalog block: %v", err)
	}

	rc, cache := testBuildContext(t, map[string]string{
		"participantId":   "agmarknet",
		"networkId":       "oan-dev",
		"fromDate":        "23-09-2026",
		"toDate":          "23-09-2026",
		"catalogOut":      "catalog",
		"withoutGeometry": "publish",
	})

	market := func(state string, id float64, quality string, commodities int, located bool) map[string]any {
		record := map[string]any{
			"stateCode": state, "stateName": state + " state", "marketId": id,
			"coordinateQuality": quality,
		}
		list := make([]any, 0, commodities)
		for i := 0; i < commodities; i++ {
			list = append(list, map[string]any{"code": float64(i), "name": "crop"})
		}
		record["commodities"] = list
		if located {
			record["latitude"] = 19.1
			record["longitude"] = 72.8
		}
		return record
	}

	records := []map[string]any{
		market("MH", 3, "ok", 1, true),
		market("MH", 1, "ok", 1, true),
		market("MH", 2, "ok", 1, true),
		market("MH", 4, "missing", 1, false),
		market("MH", 5, "ok", 0, true),
		market("KA", 9, "suspect", 2, false),
	}

	built, counters, err := buildCatalogs(context.Background(), catalog, records, rc, cache, &echoMapper{}, "http://mappings/pipeline.yaml")
	if err != nil {
		t.Fatalf("buildCatalogs: %v", err)
	}

	// KA first, then MH split at the two-geometry budget: markets 1 and 2
	// carry a geometry each, 3 opens the second chunk, and 4 (no geometry)
	// rides along for free.
	wantSlugs := []string{"KA", "MH", "MH-2"}
	if len(built) != len(wantSlugs) {
		t.Fatalf("built %d catalogs, want %d", len(built), len(wantSlugs))
	}
	for i, want := range wantSlugs {
		if built[i].Slug != want {
			t.Fatalf("catalog %d is %q, want %q", i, built[i].Slug, want)
		}
		if built[i].CatalogID != "catalog:mandi-price:"+want {
			t.Errorf("catalog %d id is %q", i, built[i].CatalogID)
		}
	}

	first, local := decodeEcho(t, built[1].Content)
	if len(first) != 2 {
		t.Errorf("MH's first chunk carries %d records, want 2", len(first))
	}
	if local["stateName"] != "MH state" || local["publishWithoutGeometry"] != "publish" {
		t.Errorf("_local = %v", local)
	}
	if local["transactionId"] == "" || local["generatedAt"] == "" {
		t.Errorf("built-ins did not resolve: %v", local)
	}

	second, _ := decodeEcho(t, built[2].Content)
	if len(second) != 2 {
		t.Errorf("MH's second chunk carries %d records, want 2", len(second))
	}
	if second[1].(map[string]any)["publishedWithoutLocation"] != true {
		t.Errorf("the geometry-less market was not annotated: %v", second[1])
	}

	if counters["excluded:upstream reported no commodities trading in this window"] != 1 {
		t.Errorf("counters = %v", counters)
	}
	if counters["annotated:publishedWithoutLocation"] != 2 {
		t.Errorf("counters = %v", counters)
	}
}

func TestWriteCatalogsWritesOneFilePerSlug(t *testing.T) {
	built := []BuiltCatalog{
		{Slug: "MH", CatalogID: "catalog:example:MH", Content: []byte(`{"context":{"a":1}}`)},
		{Slug: "MH-2", CatalogID: "catalog:example:MH-2", Content: []byte(`{"context":{"a":2}}`)},
		{Slug: "KA", CatalogID: "catalog:example:KA", Content: []byte(`{"context":{"a":3}}`)},
	}

	// A directory that does not exist yet: WriteCatalogs is what a fresh run
	// relies on to create its output directory.
	dir := filepath.Join(t.TempDir(), "catalog")
	if err := WriteCatalogs(built, dir, "example"); err != nil {
		t.Fatalf("WriteCatalogs: %v", err)
	}

	for _, name := range []string{"example-KA.json", "example-MH.json", "example-MH-2.json"} {
		content, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var doc map[string]any
		if err := json.Unmarshal(content, &doc); err != nil {
			t.Errorf("%s is not valid JSON: %v", name, err)
		}
		// Pretty-printed for the human who reviews these files before a
		// publish, which is the only reason they hit disk at all.
		if !bytes.Contains(content, []byte("\n  \"context\"")) {
			t.Errorf("%s is not indented: %s", name, content)
		}
	}

	// And nothing else: a stale file from an earlier slug would be published
	// as though it were current.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 3 {
		t.Errorf("wrote %d files, want 3", len(entries))
	}
}

func TestWriteCatalogsRemovesStaleCatalogsFromAnEarlierRun(t *testing.T) {
	dir := t.TempDir()

	// Monday: a split state left two catalogs behind.
	stale := filepath.Join(dir, "example-MH-2.json")
	if err := os.WriteFile(stale, []byte(`{"stale":true}`), 0o644); err != nil {
		t.Fatalf("seed stale catalog: %v", err)
	}
	// Something that is not ours, in the same directory. It must survive:
	// this directory is an operator's to point wherever they like.
	bystander := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(bystander, []byte("keep me"), 0o644); err != nil {
		t.Fatalf("seed bystander: %v", err)
	}
	// Another pipeline's catalog, under its own prefix. Also must survive --
	// this is the sweep half of the two-pipelines-one-directory corruption.
	neighbour := filepath.Join(dir, "weather-KA.json")
	if err := os.WriteFile(neighbour, []byte(`{"someone else":true}`), 0o644); err != nil {
		t.Fatalf("seed neighbour: %v", err)
	}

	// Tuesday: the state fits in one catalog.
	built := []BuiltCatalog{{
		Slug:      "MH",
		CatalogID: "catalog:example:MH",
		Content:   []byte(`{"current":true}`),
	}}
	if err := WriteCatalogs(built, dir, "example"); err != nil {
		t.Fatalf("WriteCatalogs: %v", err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("yesterday's catalog survived; the publish step would post it again as today's")
	}
	if _, err := os.Stat(bystander); err != nil {
		t.Errorf("a file that was not ours was deleted: %v", err)
	}
	if _, err := os.Stat(neighbour); err != nil {
		t.Errorf("another pipeline's catalog was deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "example-MH.json")); err != nil {
		t.Errorf("today's catalog was not written: %v", err)
	}
}

// A catalog name is UPSTREAM DATA that becomes a filename. Each of these is
// a real failure, not a hypothetical: traversal walks out of the operator's
// directory, a slash makes a file the publisher's glob never finds (built,
// reported, silently never published), and case-only differences collide on
// macOS.
func TestCatalogNamesThatCannotBecomeFilesAreRefused(t *testing.T) {
	for name, slug := range map[string]string{
		"traversal":        "../../etc/passwd",
		"a slash":          "J/K",
		"a backslash":      `J\K`,
		"a leading dash":   "-weird", // allowed by shape; kept to document the boundary
		"a null-ish empty": "",
		"a space":          "North West",
		"a colon":          "ns:thing",
	} {
		t.Run(name, func(t *testing.T) {
			err := safeSlug(slug, "test")
			switch slug {
			case "-weird":
				if err != nil {
					t.Errorf("a leading dash is allowed by the shape but was refused: %v", err)
				}
			default:
				if err == nil {
					t.Errorf("%q was accepted as a catalog name", slug)
				}
			}
		})
	}

	// And the ordinary names must still pass, or the check is just breakage.
	for _, ok := range []string{"MH", "MH-2", "openagrinet.MandiPrice", "region_1"} {
		if err := safeSlug(ok, "test"); err != nil {
			t.Errorf("a legitimate name %q was refused: %v", ok, err)
		}
	}
}

// Two groups whose names differ only in case are ONE file on macOS and
// Windows. Left uncaught, the second overwrites the first on disk and the
// network gets one catalog published under two ids.
func TestGroupsDifferingOnlyInCaseAreRefused(t *testing.T) {
	records := []map[string]any{
		{"region": "mh", "id": 1},
		{"region": "MH", "id": 2},
	}
	catalog := Catalog{
		GroupBy:  "region",
		Order:    Order{By: "id"},
		Chunk:    Chunk{Budget: 10, Cost: "1", Slug: "${region}"},
		Identity: Identity{CatalogID: "catalog:example:${slug}"},
		Render:   Render{Mapping: "mappings/catalog.yaml"},
	}

	cache, err := newExprCache()
	if err != nil {
		t.Fatalf("newExprCache: %v", err)
	}
	_, _, err = buildCatalogs(context.Background(), catalog, records,
		newRunContext(map[string]string{}, "tok"), cache, &echoMapper{}, "http://mappings.test")
	if err == nil {
		t.Fatal("two groups differing only in case were both built; one overwrites the other on disk")
	}
	if !strings.Contains(err.Error(), "case") {
		t.Errorf("error %q does not explain that the collision is a case one", err)
	}
}

// An EXACT slug collision is a different mistake from a case one, and the
// error has to say which.
//
// A chunk template that ignores the chunk index renders the same slug for
// every chunk of a group that splits. Telling the author that the two "differ
// only in case" sends them looking at their group names, which are fine.
func TestAnExactSlugCollisionIsNotReportedAsACaseCollision(t *testing.T) {
	records := []map[string]any{
		{"region": "MH", "id": 1},
		{"region": "MH", "id": 2},
		{"region": "MH", "id": 3},
	}
	catalog := Catalog{
		GroupBy: "region",
		Order:   Order{By: "id"},
		// Budget 2 splits this group in two, and the template names both
		// chunks the same thing.
		Chunk:    Chunk{Budget: 2, Cost: "1", Slug: "${region}"},
		Identity: Identity{CatalogID: "catalog:example:${slug}"},
		Render:   Render{Mapping: "mappings/catalog.yaml"},
	}

	cache, err := newExprCache()
	if err != nil {
		t.Fatalf("newExprCache: %v", err)
	}
	_, _, err = buildCatalogs(context.Background(), catalog, records,
		newRunContext(map[string]string{}, "tok"), cache, &echoMapper{}, "http://mappings.test")
	if err == nil {
		t.Fatal("two chunks rendering one slug were both built; the second overwrites the first")
	}
	if strings.Contains(err.Error(), "case") {
		t.Errorf("an exact collision was reported as a case collision: %v", err)
	}
	if !strings.Contains(err.Error(), "chunk") {
		t.Errorf("error %q does not point at the chunk template that caused it", err)
	}
}

// order.by decides where a big group is SPLIT; order.renderBy decides the
// order a reader sees inside each catalog. Splitting by id keeps the chunk
// boundaries stable as markets are added (new ids land in the last chunk), so
// no market moves between catalogIds -- which, under MERGE, would leave it
// published twice.
func TestRenderByOrdersWithinEachChunkWithoutMovingTheSplit(t *testing.T) {
	records := []map[string]any{
		{"stateCode": "MH", "marketId": 1.0, "marketName": "Erandol"},
		{"stateCode": "MH", "marketId": 2.0, "marketName": "Dhule"},
		{"stateCode": "MH", "marketId": 3.0, "marketName": "chandrapur"},
		{"stateCode": "MH", "marketId": 4.0, "marketName": "Beed"},
		{"stateCode": "MH", "marketId": 5.0, "marketName": "Akola"},
	}
	rc, cache := testBuildContext(t, nil)
	catalog := simpleCatalog()
	catalog.Chunk = Chunk{Budget: 3, Cost: "1", Slug: "${stateCode}${chunkIndex > 1 ? '-' & chunkIndex : ''}"}
	catalog.Order = Order{By: "marketId", Direction: "asc", RenderBy: []string{"marketName", "marketId"}}

	built, _, err := buildCatalogs(context.Background(), catalog, records, rc, cache, &echoMapper{}, "http://mappings/pipeline.yaml")
	if err != nil {
		t.Fatalf("buildCatalogs: %v", err)
	}
	if len(built) != 2 {
		t.Fatalf("built %d catalogs, want 2", len(built))
	}
	// Chunk membership follows ids (1,2,3 | 4,5); display order follows names,
	// case-insensitively ("chandrapur" before "Dhule").
	want := [][]float64{{3, 2, 1}, {5, 4}}
	for i, ids := range want {
		response, _ := decodeEcho(t, built[i].Content)
		if len(response) != len(ids) {
			t.Fatalf("chunk %d carries %d records, want %d", i, len(response), len(ids))
		}
		for j, id := range ids {
			if got := response[j].(map[string]any)["marketId"]; got != id {
				t.Errorf("chunk %d position %d is market %v, want %v", i, j, got, id)
			}
		}
	}
}

// A group whose membership must not move between catalogIds is refused when
// it would split, rather than split: under MERGE the records that moved leave
// a stale copy behind in their old catalog. The rule is per record kind, so
// the same catalog block can still split a group that is allowed to.
func TestRefuseSplitWhenFailsALoudGroupThatWouldSplit(t *testing.T) {
	records := []map[string]any{
		{"stateCode": "MH", "kind": "direct", "marketId": 1.0},
		{"stateCode": "MH", "kind": "direct", "marketId": 2.0},
		{"stateCode": "MH", "kind": "direct", "marketId": 3.0},
		{"stateCode": "KA", "kind": "onDemand", "marketId": 4.0},
		{"stateCode": "KA", "kind": "onDemand", "marketId": 5.0},
		{"stateCode": "KA", "kind": "onDemand", "marketId": 6.0},
	}
	build := func(t *testing.T, in []map[string]any) ([]BuiltCatalog, error) {
		rc, cache := testBuildContext(t, nil)
		catalog := simpleCatalog()
		catalog.Chunk = Chunk{Budget: 2, Cost: "1", RefuseSplitWhen: "kind = 'direct'",
			Slug: "${stateCode}${chunkIndex > 1 ? '-' & chunkIndex : ''}"}
		catalog.Order = Order{By: "marketId", Direction: "asc"}
		built, _, err := buildCatalogs(context.Background(), catalog, in, rc, cache, &echoMapper{}, "http://mappings/pipeline.yaml")
		return built, err
	}

	if _, err := build(t, records[:3]); err == nil || !strings.Contains(err.Error(), "refuseSplitWhen") || !strings.Contains(err.Error(), `"MH"`) {
		t.Fatalf("err = %v, want the direct group that would split refused, naming the group and the rule", err)
	}
	built, err := build(t, records[3:])
	if err != nil {
		t.Fatalf("a group the rule does not claim was refused: %v", err)
	}
	if len(built) != 2 {
		t.Fatalf("built %d catalogs, want the onDemand group split in two", len(built))
	}
	// A direct group that fits is untouched.
	if built, err = build(t, records[:2]); err != nil || len(built) != 1 {
		t.Fatalf("a direct group within budget: built %d, err %v; want 1 catalog", len(built), err)
	}
}

// Two markets with one name are ordered by the next key, numerically.
func TestRenderByBreaksTiesOnTheNextKey(t *testing.T) {
	records := []map[string]any{
		{"stateCode": "KA", "marketId": 305.0, "marketName": "Hubli"},
		{"stateCode": "KA", "marketId": 201.0, "marketName": "Hubli"},
		{"stateCode": "KA", "marketId": 9.0, "marketName": "Akola"},
		{"stateCode": "KA", "marketId": 10.0, "marketName": "Hubli"},
	}
	rc, cache := testBuildContext(t, nil)
	catalog := simpleCatalog()
	catalog.Order = Order{By: "marketId", RenderBy: []string{"marketName", "marketId"}}

	built, _, err := buildCatalogs(context.Background(), catalog, records, rc, cache, &echoMapper{}, "http://mappings/pipeline.yaml")
	if err != nil {
		t.Fatalf("buildCatalogs: %v", err)
	}
	response, _ := decodeEcho(t, built[0].Content)
	for i, want := range []float64{9, 10, 201, 305} {
		if got := response[i].(map[string]any)["marketId"]; got != want {
			t.Errorf("position %d is market %v, want %v", i, got, want)
		}
	}
}
