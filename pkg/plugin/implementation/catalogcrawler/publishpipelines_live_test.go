package catalogcrawler

// publishpipelines_live_test.go watches the crawler's OWN tick against the
// real registry, and stops before it fetches anything.
//
// The pipeline package has its own live tests; what this one shows is the half
// the crawler owns: reading its config, refusing a capability it carries no
// pipeline for, consulting the registry, and handing the record over -- with
// the crawler's real log lines, in order.
//
// Skipped unless MANDI_LIVE=1, so `go test ./...` stays hermetic.
//
//	MANDI_LIVE=1 SUNBIRD_REGISTRY_URL=http://localhost:8081/api/v1 \
//	  go test ./pkg/plugin/implementation/catalogcrawler/ \
//	  -run TestLive_CrawlerTick -count=1 -v

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
	agmarket "github.com/beckn-one/beckn-onix/pkg/plugin/implementation/MandiPrice/cataloguepublish-agmarket"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogcrawler/internal/store"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogpublisher/pipeline"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/sunbirdRegistry"
	"github.com/beckn/catalog-core/pkg/catalog/crawlmanager"
)

func TestLive_CrawlerTick(t *testing.T) {
	if os.Getenv("MANDI_LIVE") != "1" {
		t.Skip("live test: set MANDI_LIVE=1 to run against the real registry")
	}
	registryURL := os.Getenv("SUNBIRD_REGISTRY_URL")
	if registryURL == "" {
		// Deliberately no default: from a shell it is localhost, from a
		// container it is not, and guessing wrong fails as a DNS error far
		// from the real problem.
		t.Skip("set SUNBIRD_REGISTRY_URL (from a shell: http://localhost:8081/api/v1)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// Logs to stderr in the order the crawler emits them -- the point of this
	// test is to be read, not just to pass.
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	t.Log("STEP 1 — read the crawler's config")
	bindingKey := os.Getenv("MANDI_LIVE_BINDING_KEY")
	if bindingKey == "" {
		bindingKey = "agmarknet-live|" + agmarket.Capability
	}
	cfg, err := publishConfigFrom(map[string]string{
		cfgPublishBindingKeys: bindingKey,
		// Publishing stays off. This test reads and decides; it never sends.
		cfgPublishEnabled: "false",
	})
	if err != nil {
		t.Fatalf("config refused: %v", err)
	}
	t.Logf("  enabled=%v bindingKeys=%v tick=%s publish=%v",
		cfg.enabled, cfg.bindingKeys, cfg.tick, cfg.publish)
	t.Logf("  this binary carries pipelines for: %s", knownCapabilities())

	t.Log("STEP 2 — connect to the registry")
	registry, closeRegistry, err := sunbirdRegistry.New(ctx, nil, &sunbirdRegistry.Config{
		URL:            registryURL,
		Entity:         "Participant",
		ProviderEntity: "ProviderSchema",
		Timeout:        10,
	})
	if err != nil {
		t.Fatalf("sunbirdRegistry.New: %v", err)
	}
	defer func() { _ = closeRegistry() }()

	t.Log("STEP 3 — build the crawler's runners (one per binding key)")
	runners, err := newPublishRunners(cfg, registry, nil, log)
	if err != nil {
		t.Fatalf("newPublishRunners: %v", err)
	}
	if len(runners) != 1 {
		t.Fatalf("got %d runners, want 1", len(runners))
	}
	runner := runners[0]
	t.Logf("  runner: bindingKey=%s capability=%s pipeline=%s",
		runner.bindingKey, runner.capability, runner.files.RegistryPath)

	// STEP 4 — the tick itself, with the pipeline stopped at the schedule.
	//
	// The runner's real work is swapped for a DRY RUN: everything up to and
	// including "is it due" happens for real -- the registry lookup, the
	// publish-action gate, the pipeline load, the cron decision -- and nothing
	// is fetched. That is exactly the half worth watching here.
	var report pipeline.RunReport
	var runErr error
	runner.run = func(ctx context.Context, record *model.ProviderRecord) error {
		t.Log("STEP 5 — the registry answered; these are its actions")
		for action, plan := range record.Actions {
			t.Logf("    %-9q -> %-4s %-32s mappings: %s", action, plan.Method, plan.Path, plan.Mappings)
		}

		t.Log("STEP 6 — the gate: which of those sanctions a pipeline")
		path, err := pipeline.PipelinePathFor(record)
		if err != nil {
			t.Fatalf("    the gate refused: %v", err)
		}
		t.Logf("    publish action names: %s", path)

		t.Log("STEP 7 — load that pipeline and ask whether it is due")
		report, runErr = pipeline.Run(ctx, pipeline.RunOptions{
			Pipeline: runner.files,
			Record:   record,
			Now:      time.Now(),
			DryRun:   true, // stop here: decide, do not fetch
			Log:      log,
		})
		return runErr
	}

	t.Log("STEP 4 — tick")
	runner.tick(ctx)

	if runErr != nil {
		t.Fatalf("the tick failed: %v", runErr)
	}
	t.Log("STEP 8 — the decision")
	t.Logf("    capability   %s", report.Capability)
	t.Logf("    pipeline     %s", report.PipelinePath)
	t.Logf("    due          %v", report.Due)
	t.Logf("    reason       %s", report.Reason)
	t.Logf("    unpersisted  %v  (no run log wired in this test, so every tick looks like the first)",
		report.Unpersisted)

	if report.PipelinePath == "" {
		t.Error("the tick reached no pipeline")
	}
	if len(report.Catalogues) != 0 {
		t.Errorf("a dry run produced %d catalogues; it should have fetched nothing", len(report.Catalogues))
	}
}

// A capability this binary carries no pipeline for must be refused at config
// time -- at startup, naming what IS available -- not left as a tick that
// quietly does nothing every five minutes forever.
func TestLive_CrawlerRefusesAnUnknownCapabilityAtStartup(t *testing.T) {
	if os.Getenv("MANDI_LIVE") != "1" {
		t.Skip("live test: set MANDI_LIVE=1")
	}
	_, err := publishConfigFrom(map[string]string{
		cfgPublishBindingKeys: "someone|openagrinet:NotCarriedHere",
	})
	if err == nil {
		t.Fatal("a capability with no compiled-in pipeline was accepted")
	}
	t.Logf("refused at startup, as it should be:\n  %v", err)
}

// TestLive_CrawlerBuildsCatalogues is the same tick, allowed to finish.
//
// Everything runs for real against Agmarknet -- the token exchange, the state
// list, the master market list, thirty-six per-state calls, the join, the
// coordinate verdicts, the grouping and chunking, and the catalogue render --
// and the result is written to disk. Publishing stays OFF, so nothing leaves
// this machine.
//
// It is slow by nature: the per-state calls are sequential, because the file
// says concurrency: 1 and the runner refuses to pretend otherwise.
//
//	MANDI_LIVE=1 SUNBIRD_REGISTRY_URL=http://localhost:8081/api/v1 \
//	  MANDI_LIVE_OUT=/tmp/mandi \
//	  go test ./pkg/plugin/implementation/catalogcrawler/ \
//	  -run TestLive_CrawlerBuildsCatalogues -count=1 -v -timeout 20m
func TestLive_CrawlerBuildsCatalogues(t *testing.T) {
	if os.Getenv("MANDI_LIVE") != "1" {
		t.Skip("live test: set MANDI_LIVE=1 to run against the real services")
	}
	registryURL := os.Getenv("SUNBIRD_REGISTRY_URL")
	if registryURL == "" {
		t.Skip("set SUNBIRD_REGISTRY_URL (from a shell: http://localhost:8081/api/v1)")
	}

	outDir := os.Getenv("MANDI_LIVE_OUT")
	if outDir == "" {
		outDir = t.TempDir()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	bindingKey := os.Getenv("MANDI_LIVE_BINDING_KEY")
	if bindingKey == "" {
		bindingKey = "agmarknet-live|" + agmarket.Capability
	}
	cfg, err := publishConfigFrom(map[string]string{
		cfgPublishBindingKeys: bindingKey,
		// OFF. This test reads from the upstream and writes to disk. It does
		// not put anything on the network.
		cfgPublishEnabled:          "false",
		cfgPublishCatalogOutputDir: outDir,
	})
	if err != nil {
		t.Fatalf("config refused: %v", err)
	}

	registry, closeRegistry, err := sunbirdRegistry.New(ctx, nil, &sunbirdRegistry.Config{
		URL: registryURL, Entity: "Participant", ProviderEntity: "ProviderSchema", Timeout: 10,
	})
	if err != nil {
		t.Fatalf("sunbirdRegistry.New: %v", err)
	}
	defer func() { _ = closeRegistry() }()

	runners, err := newPublishRunners(cfg, registry, nil, log)
	if err != nil {
		t.Fatalf("newPublishRunners: %v", err)
	}
	runner := runners[0]

	// The runner's real call, captured so the report can be read afterwards.
	var report pipeline.RunReport
	var runErr error
	realRun := runner.run
	runner.run = func(ctx context.Context, record *model.ProviderRecord) error {
		report, runErr = pipeline.Run(ctx, pipeline.RunOptions{
			Pipeline: runner.files,
			Record:   record,
			OutDir:   cfg.outDir,
			Publish:  cfg.publish, // false
			Now:      time.Now(),
			Log:      log,
		})
		return runErr
	}
	_ = realRun

	started := time.Now()
	t.Logf("tick starting; the per-state calls are sequential, so this takes minutes")
	runner.tick(ctx)
	elapsed := time.Since(started)

	if runErr != nil {
		t.Fatalf("the run failed after %s: %v", elapsed.Round(time.Second), runErr)
	}
	if !report.Due {
		t.Skipf("not due, so nothing ran: %s", report.Reason)
	}

	t.Logf("ran in %s", elapsed.Round(time.Second))
	t.Logf("catalogues: %d, written to %s", len(report.Catalogues), report.OutDir)

	// The counters the FILE named, plus the per-step sizes the runner records.
	names := make([]string, 0, len(report.Counters))
	for name := range report.Counters {
		names = append(names, name)
	}
	sort.Strings(names)
	t.Log("counters:")
	for _, name := range names {
		t.Logf("    %-38s %d", name, report.Counters[name])
	}

	t.Log("catalogues:")
	for i, catalogue := range report.Catalogues {
		if i == 8 {
			t.Logf("    ... and %d more", len(report.Catalogues)-8)
			break
		}
		t.Logf("    %-8s %-34s %7d bytes", catalogue.Slug, catalogue.CatalogID, len(catalogue.Content))
	}

	if len(report.Catalogues) == 0 {
		t.Error("the live upstream produced no catalogues at all")
	}
	if report.Errors > 0 {
		t.Errorf("%d parts of the collection failed; a real run would refuse to publish", report.Errors)
	}
	if report.Published != nil {
		t.Error("publishing was off, but a publish result came back")
	}
}

// TestLive_CrawlerPublishesThenDeclines is the whole thing, twice.
//
// Tick 1 runs the pipeline for real and PUTS CATALOGUES ON THE NETWORK.
// Tick 2 fires immediately afterwards and must decline, because the run log
// -- a real row in Postgres, written by tick 1 -- says this firing has already
// been served. That second tick is the point: without it, a crawler restarting
// at 00:05 would republish everything it published at 00:01.
//
// THIS ONE PUBLISHES. It needs its own opt-in for that reason, and a database,
// because a run log that does not outlive the process proves nothing:
//
//	MANDI_LIVE=1 MANDI_LIVE_PUBLISH=1 \
//	  SUNBIRD_REGISTRY_URL=http://localhost:8081/api/v1 \
//	  CATALOGCRAWLER_DSN='postgres://mandi:mandi@localhost:55432/catalogcrawler?sslmode=disable' \
//	  MANDI_FROM_DATE=23-06-2026 MANDI_TO_DATE=23-09-2026 \
//	  go test ./pkg/plugin/implementation/catalogcrawler/ \
//	  -run TestLive_CrawlerPublishesThenDeclines -count=1 -v -timeout 20m
func TestLive_CrawlerPublishesThenDeclines(t *testing.T) {
	if os.Getenv("MANDI_LIVE") != "1" {
		t.Skip("live test: set MANDI_LIVE=1")
	}
	if os.Getenv("MANDI_LIVE_PUBLISH") != "1" {
		t.Skip("this test PUBLISHES to the network: set MANDI_LIVE_PUBLISH=1 to allow it")
	}
	registryURL := os.Getenv("SUNBIRD_REGISTRY_URL")
	dsn := os.Getenv("CATALOGCRAWLER_DSN")
	if registryURL == "" || dsn == "" {
		t.Skip("set SUNBIRD_REGISTRY_URL and CATALOGCRAWLER_DSN")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// The real store, migrated exactly as Provider.New migrates it -- so the
	// run log under test is the one production uses, table and all.
	db, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("opening the database: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	runLog := store.New(db)

	bindingKey := os.Getenv("MANDI_LIVE_BINDING_KEY")
	if bindingKey == "" {
		bindingKey = "agmarknet-live|" + agmarket.Capability
	}
	outDir := os.Getenv("MANDI_LIVE_OUT")
	if outDir == "" {
		outDir = t.TempDir()
	}

	cfg, err := publishConfigFrom(map[string]string{
		cfgPublishBindingKeys:      bindingKey,
		cfgPublishEnabled:          "true", // <- the network
		cfgPublishCatalogOutputDir: outDir,
	})
	if err != nil {
		t.Fatalf("config refused: %v", err)
	}

	registry, closeRegistry, err := sunbirdRegistry.New(ctx, nil, &sunbirdRegistry.Config{
		URL: registryURL, Entity: "Participant", ProviderEntity: "ProviderSchema", Timeout: 10,
	})
	if err != nil {
		t.Fatalf("sunbirdRegistry.New: %v", err)
	}
	defer func() { _ = closeRegistry() }()

	runners, err := newPublishRunners(cfg, registry, runLog, log)
	if err != nil {
		t.Fatalf("newPublishRunners: %v", err)
	}
	runner := runners[0]

	// Capture each tick's report without changing what the runner does.
	var reports []pipeline.RunReport
	var errs []error
	runner.run = func(ctx context.Context, record *model.ProviderRecord) error {
		report, err := pipeline.Run(ctx, pipeline.RunOptions{
			Pipeline: runner.files,
			Record:   record,
			RunLog:   runLog,
			OutDir:   cfg.outDir,
			Publish:  cfg.publish,
			Now:      time.Now(),
			Log:      log,
		})
		reports = append(reports, report)
		errs = append(errs, err)
		return err
	}

	// Start from a clean slate, so tick 1 is genuinely the first of its
	// firing rather than inheriting an earlier experiment's row.
	if err := runLog.RecordPipelineRun(ctx, agmarket.Capability, time.Time{}); err != nil {
		t.Fatalf("clearing the run log: %v", err)
	}

	t.Log("================ TICK 1 — should run and publish ================")
	started := time.Now()
	runner.tick(ctx)
	t.Logf("tick 1 took %s", time.Since(started).Round(time.Second))

	if len(reports) != 1 {
		t.Fatalf("tick 1 produced %d reports, want 1", len(reports))
	}
	first := reports[0]
	if errs[0] != nil {
		t.Fatalf("tick 1 failed: %v", errs[0])
	}
	if !first.Due {
		t.Fatalf("tick 1 was not due: %s", first.Reason)
	}
	t.Logf("  due     %v — %s", first.Due, first.Reason)
	t.Logf("  built   %d catalogues", len(first.Catalogues))
	if first.Published == nil {
		t.Fatal("  publishing was on, but no publish result came back")
	}
	t.Logf("  PUBLISHED %d outcomes:", len(first.Published.Outcomes))
	for _, outcome := range first.Published.Outcomes {
		t.Logf("      %-8s %-32s %-10s %s",
			outcome.StateCode, outcome.CatalogID, outcome.Status, outcome.Reason)
	}
	if first.Published.HasFailures() {
		t.Error("  at least one catalogue did not reach the network intact " +
			"(PARTIAL counts: the catalogue indexed with resources missing)")
	}

	// What tick 2 will read.
	recorded, err := runLog.LastPipelineRun(ctx, agmarket.Capability)
	if err != nil {
		t.Fatalf("reading the run log: %v", err)
	}
	t.Logf("  run log now says: last ran %s", recorded.Format(time.RFC3339))

	t.Log("================ TICK 2 — should decline ================")
	runner.tick(ctx)

	if len(reports) != 2 {
		t.Fatalf("tick 2 produced no report (got %d in total)", len(reports))
	}
	second := reports[1]
	if errs[1] != nil {
		t.Fatalf("tick 2 failed: %v", errs[1])
	}
	t.Logf("  due     %v — %s", second.Due, second.Reason)

	if second.Due {
		t.Error("tick 2 ran again; a restart would republish the whole day")
	}
	if len(second.Catalogues) != 0 {
		t.Errorf("tick 2 built %d catalogues; it should have fetched nothing", len(second.Catalogues))
	}
	if second.Published != nil {
		t.Error("tick 2 published; the run log did not stop it")
	}
}

// idleSource discovers nothing, so the crawler's own index-poll and
// catalog-sync loops run for real and find no work. The publish pipeline is
// what this test watches; the crawl loops are present because the Scheduler
// really does run them alongside it, and pretending otherwise would test a
// shape production does not have.
type idleSource struct{}

func (idleSource) Discover(context.Context) ([]crawlmanager.IndexRef, error) { return nil, nil }

// TestLive_CrawlerTicksOnItsOwnClock starts the REAL Scheduler and watches it.
//
// Nothing here calls tick() by hand. The Scheduler's own ticker fires it: once
// immediately, then every interval. The first firing runs the pipeline and
// publishes; every later firing must find the run log already satisfied for
// this cron firing and stand down. What you are watching is the wait.
//
//	MANDI_LIVE=1 MANDI_LIVE_PUBLISH=1 \
//	  SUNBIRD_REGISTRY_URL=http://localhost:8081/api/v1 \
//	  CATALOGCRAWLER_DSN='postgres://mandi:mandi@localhost:55432/catalogcrawler?sslmode=disable' \
//	  MANDI_FROM_DATE=23-06-2026 MANDI_TO_DATE=23-09-2026 \
//	  MANDI_TICK_SECONDS=120 MANDI_TICKS=3 \
//	  go test ./pkg/plugin/implementation/catalogcrawler/ \
//	  -run TestLive_CrawlerTicksOnItsOwnClock -count=1 -v -timeout 20m
func TestLive_CrawlerTicksOnItsOwnClock(t *testing.T) {
	if os.Getenv("MANDI_LIVE") != "1" {
		t.Skip("live test: set MANDI_LIVE=1")
	}
	registryURL := os.Getenv("SUNBIRD_REGISTRY_URL")
	dsn := os.Getenv("CATALOGCRAWLER_DSN")
	if registryURL == "" || dsn == "" {
		t.Skip("set SUNBIRD_REGISTRY_URL and CATALOGCRAWLER_DSN")
	}
	publishing := os.Getenv("MANDI_LIVE_PUBLISH") == "1"

	interval := 120 * time.Second
	if raw := os.Getenv("MANDI_TICK_SECONDS"); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds < 1 {
			t.Fatalf("MANDI_TICK_SECONDS=%q is not a positive number of seconds", raw)
		}
		interval = time.Duration(seconds) * time.Second
	}
	wantTicks := 3
	if raw := os.Getenv("MANDI_TICKS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 2 {
			t.Fatalf("MANDI_TICKS=%q must be at least 2 (one run, one stand-down)", raw)
		}
		wantTicks = n
	}

	// Long enough for the first run plus the remaining waits, with slack.
	budget := time.Duration(wantTicks)*interval + 8*time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	db, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("opening the database: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	st := store.New(db)

	bindingKey := os.Getenv("MANDI_LIVE_BINDING_KEY")
	if bindingKey == "" {
		bindingKey = "agmarknet-live|" + agmarket.Capability
	}
	outDir := os.Getenv("MANDI_LIVE_OUT")
	if outDir == "" {
		outDir = t.TempDir()
	}

	cfg, err := publishConfigFrom(map[string]string{
		cfgPublishBindingKeys:      bindingKey,
		cfgPublishEnabled:          map[bool]string{true: "true", false: "false"}[publishing],
		cfgPublishCatalogOutputDir: outDir,
		cfgPublishTickIntervalSec:  strconv.Itoa(int(interval.Seconds())),
	})
	if err != nil {
		t.Fatalf("config refused: %v", err)
	}

	registry, closeRegistry, err := sunbirdRegistry.New(ctx, nil, &sunbirdRegistry.Config{
		URL: registryURL, Entity: "Participant", ProviderEntity: "ProviderSchema", Timeout: 10,
	})
	if err != nil {
		t.Fatalf("sunbirdRegistry.New: %v", err)
	}
	defer func() { _ = closeRegistry() }()

	runners, err := newPublishRunners(cfg, registry, st, log)
	if err != nil {
		t.Fatalf("newPublishRunners: %v", err)
	}
	runner := runners[0]

	// Watch each firing without changing what it does.
	type firing struct {
		at       time.Time
		due      bool
		reason   string
		built    int
		outcomes int
		err      error
	}
	var mu sync.Mutex
	var firings []firing
	done := make(chan struct{})

	realRun := runner.runPipeline
	runner.run = func(ctx context.Context, record *model.ProviderRecord) error {
		at := time.Now()
		report, runErr := pipeline.Run(ctx, pipeline.RunOptions{
			Pipeline: runner.files,
			Record:   record,
			RunLog:   st,
			OutDir:   cfg.outDir,
			Publish:  cfg.publish,
			Now:      time.Now(),
			Log:      log,
		})
		outcomes := 0
		if report.Published != nil {
			outcomes = len(report.Published.Outcomes)
		}

		mu.Lock()
		firings = append(firings, firing{at, report.Due, report.Reason, len(report.Catalogues), outcomes, runErr})
		n := len(firings)
		mu.Unlock()

		t.Logf("──────── FIRING %d at %s ────────", n, at.Format("15:04:05"))
		t.Logf("    due       %v", report.Due)
		t.Logf("    reason    %s", report.Reason)
		t.Logf("    built     %d catalogues", len(report.Catalogues))
		t.Logf("    published %d outcomes", outcomes)
		if runErr != nil {
			t.Logf("    ERROR     %v", runErr)
		}
		if n >= wantTicks {
			close(done)
		}
		return runErr
	}
	_ = realRun

	// A clean slate, so firing 1 is genuinely the first of its cron firing.
	if err := st.RecordPipelineRun(ctx, agmarket.Capability, time.Time{}); err != nil {
		t.Fatalf("clearing the run log: %v", err)
	}

	// The real Scheduler, with the crawl loops it always carries. Their
	// cadences are pushed out of the way so the output is the publish
	// pipeline's; they still run once at startup, as they do in production.
	sched := NewScheduler(crawlmanager.Params{Store: st, Source: idleSource{}, Log: log},
		SchedulerConfig{
			IndexInterval:     24 * time.Hour,
			CatalogInterval:   24 * time.Hour,
			ParkSweepInterval: 24 * time.Hour,
		}, log)
	if err := sched.AddPeriodic(cfg.tick, runner.tick); err != nil {
		t.Fatalf("AddPeriodic: %v", err)
	}

	t.Logf("starting the scheduler: publish tick every %s, watching for %d firings", cfg.tick, wantTicks)
	t.Logf("publishing is %s", map[bool]string{true: "ON — catalogues will reach the network", false: "OFF"}[publishing])
	started := time.Now()
	sched.Start(ctx)
	defer sched.Stop()

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("only %d firings in %s; wanted %d", len(firings), time.Since(started).Round(time.Second), wantTicks)
	}

	mu.Lock()
	defer mu.Unlock()

	t.Log("════════ SUMMARY ════════")
	for i, f := range firings {
		gap := ""
		if i > 0 {
			gap = " (+" + time.Since(firings[i-1].at).Round(time.Second).String() + " after the previous)"
			gap = " (+" + f.at.Sub(firings[i-1].at).Round(time.Second).String() + " after the previous)"
		}
		t.Logf("  firing %d  %s  due=%-5v built=%-3d published=%-3d%s",
			i+1, f.at.Format("15:04:05"), f.due, f.built, f.outcomes, gap)
	}

	// Firing 1 does the work.
	if !firings[0].due {
		t.Errorf("the first firing was not due: %s", firings[0].reason)
	}
	if firings[0].err != nil {
		t.Errorf("the first firing failed: %v", firings[0].err)
	}
	if firings[0].built == 0 {
		t.Error("the first firing built nothing")
	}

	// Every later firing stands down, because the run log says this cron
	// firing has been served. This is what stops a restart republishing.
	for i, f := range firings[1:] {
		if f.due {
			t.Errorf("firing %d ran again; a restart would republish the whole day: %s", i+2, f.reason)
		}
		if f.built != 0 {
			t.Errorf("firing %d built %d catalogues; it should have fetched nothing", i+2, f.built)
		}
	}

	// And the waits were real, not a burst.
	for i := 1; i < len(firings); i++ {
		gap := firings[i].at.Sub(firings[i-1].at)
		if gap < interval/2 {
			t.Errorf("firing %d came %s after the previous, but the interval is %s", i+1, gap, interval)
		}
	}
}

// TestLive_SurveyRegistryForPublishActions walks EVERY capability the registry
// holds and shows what the publish gate would say about each.
//
// This is the question "which of these publishes, and can this binary serve
// it?" asked of the real registry. The three answers it distinguishes are the
// three that matter, and they are easy to confuse from outside:
//
//	SELECT-ONLY   the registry lists no publish action. Normal, not broken:
//	              most capabilities are consumed, never published.
//	NO PIPELINE   the registry sanctions publishing, but this binary carries
//	              no pipeline for it. A deployment problem.
//	READY         sanctioned, and compiled in.
//
//	MANDI_LIVE=1 SUNBIRD_REGISTRY_URL=http://localhost:8081/api/v1 \
//	  go test ./pkg/plugin/implementation/catalogcrawler/ \
//	  -run TestLive_SurveyRegistryForPublishActions -count=1 -v
func TestLive_SurveyRegistryForPublishActions(t *testing.T) {
	if os.Getenv("MANDI_LIVE") != "1" {
		t.Skip("live test: set MANDI_LIVE=1")
	}
	registryURL := os.Getenv("SUNBIRD_REGISTRY_URL")
	if registryURL == "" {
		t.Skip("set SUNBIRD_REGISTRY_URL (from a shell: http://localhost:8081/api/v1)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// Enumerating every record is not something the plugin's interface does --
	// it answers one binding key at a time, which is all a run needs. So the
	// LIST comes from the registry's own search, and each key is then resolved
	// through the plugin, so the gate is exercised exactly as a tick does it.
	keys := listBindingKeys(ctx, t, registryURL)
	if len(keys) == 0 {
		t.Skip("the registry holds no ProviderSchema records")
	}

	registry, closeRegistry, err := sunbirdRegistry.New(ctx, nil, &sunbirdRegistry.Config{
		URL: registryURL, Entity: "Participant", ProviderEntity: "ProviderSchema", Timeout: 10,
	})
	if err != nil {
		t.Fatalf("sunbirdRegistry.New: %v", err)
	}
	defer func() { _ = closeRegistry() }()

	t.Logf("the registry holds %d capabilities; this binary carries pipelines for %s",
		len(keys), knownCapabilities())

	ready, selectOnly, unserveable := 0, 0, 0
	for _, key := range keys {
		t.Logf("──────── %s ────────", key)

		record, err := registry.ProviderRecord(ctx, key)
		if err != nil {
			t.Logf("    registry could not resolve it: %v", err)
			continue
		}

		served := make([]string, 0, len(record.Actions))
		for action := range record.Actions {
			served = append(served, action)
		}
		sort.Strings(served)
		t.Logf("    actions        %s", strings.Join(served, ", "))

		// The same gate a tick uses, on the same record.
		path, gateErr := pipeline.PipelinePathFor(record)
		if gateErr != nil {
			selectOnly++
			t.Logf("    gate           SELECT-ONLY — nothing to publish")
			t.Logf("                   %v", gateErr)
			continue
		}
		t.Logf("    publish names  %s", path)

		if _, carried := collectors[record.CapabilityCode]; !carried {
			unserveable++
			t.Logf("    gate           NO PIPELINE — the registry sanctions publishing, but")
			t.Logf("                   this binary carries none for %s", record.CapabilityCode)
			continue
		}
		ready++
		t.Logf("    gate           READY — sanctioned and compiled in")
	}

	t.Log("════════ SUMMARY ════════")
	t.Logf("  READY        %d", ready)
	t.Logf("  SELECT-ONLY  %d  (normal: most capabilities are consumed, not published)", selectOnly)
	t.Logf("  NO PIPELINE  %d  (sanctioned to publish, but this binary cannot)", unserveable)

	if ready == 0 {
		t.Error("no capability in this registry is both sanctioned to publish and compiled in")
	}
}

// listBindingKeys asks the registry for every ProviderSchema record it holds.
func listBindingKeys(ctx context.Context, t *testing.T, registryURL string) []string {
	t.Helper()

	body := strings.NewReader(`{"filters":{}}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(registryURL, "/")+"/ProviderSchema/search", body)
	if err != nil {
		t.Fatalf("building the search request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("searching the registry: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the registry search returned %s", resp.Status)
	}

	var page struct {
		Data []struct {
			BindingKey string `json:"bindingKey"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatalf("decoding the search result: %v", err)
	}

	keys := make([]string, 0, len(page.Data))
	for _, record := range page.Data {
		if record.BindingKey != "" {
			keys = append(keys, record.BindingKey)
		}
	}
	sort.Strings(keys)
	return keys
}
