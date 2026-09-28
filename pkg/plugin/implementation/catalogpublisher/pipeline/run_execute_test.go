package pipeline

// run_execute_test.go covers the half of a run that run_test.go stops short
// of: everything after the schedule says "go".
//
// It runs the fixture pipeline in testdata/ against a fake upstream, through
// the real interpreter and the real catalog builder. Nothing here is stubbed
// except the upstream itself, so what is under test is the path production
// takes.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

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
		record.Actions["publish"] = model.ActionPlan{Mappings: files.RegistryPath}

		report, err := Run(context.Background(), RunOptions{
			Pipeline: files,
			Record:   record,
			Lookup:   fixtureEnv(upstream.URL),
			Now:      now,
			OutDir:   shared,
		})
		if err != nil {
			t.Fatalf("Run(%s): %v", files.Path, err)
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
		Schedule: Schedule{Cron: "0 0 * * *", Timezone: "Asia/Kolkata"},
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
		Schedule: Schedule{Timezone: "Asia/Kolkata"},
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

func (f *claimingRunLog) ReleasePipelineRun(_ context.Context, key string, previous time.Time) error {
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
	key := fixturePipeline().Path
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
	key := fixturePipeline().Path
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
		Schedule: Schedule{Timezone: "Asia/Kolkata"},
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
	rc.utcOffset = offsetIn("Asia/Kolkata", time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC))
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
		Schedule: Schedule{Cron: "0 0 * * *", Timezone: "Asia/Kolkata"},
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
