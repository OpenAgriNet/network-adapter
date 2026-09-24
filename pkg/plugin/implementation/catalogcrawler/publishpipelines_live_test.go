package catalogcrawler

// publishpipelines_live_test.go watches the crawler's OWN publish sweep
// against the real registry.
//
// The pipeline package has its own tests; what this one shows is the half the
// crawler owns: listing the registry, resolving each binding, picking out the
// ones with a publish action, finding the YAML each names, and running it --
// with the crawler's real log lines, in order.
//
// EVERY binding with a publish action and an embedded YAML runs, exactly as a
// production tick does. MANDI_LIVE_BINDING_KEY, when set, narrows a run to one
// binding; unset, nothing is filtered.
//
// Every pipeline publishes to the one address in CATALOG_PUBLISH_URL. Beyond
// that each reads its own environment: mandi needs MANDI_TOKEN_USER and
// MANDI_TOKEN_SECRET; weather needs nothing more (it has no upstream). A
// pipeline missing its env fails its own run and is reported; it does not
// stop the others.
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
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogcrawler/internal/store"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogpublisher/pipeline"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/sunbirdRegistry"
	"github.com/beckn/catalog-core/pkg/catalog/crawlmanager"
)

// liveFilter is MANDI_LIVE_BINDING_KEY: one binding to narrow a run to, or ""
// for every binding the sweep reaches.
func liveFilter() string { return strings.TrimSpace(os.Getenv("MANDI_LIVE_BINDING_KEY")) }

// watched wraps a sweep's run so the filter, if any, is honoured, and every
// binding the sweep hands over is announced.
func watched(t *testing.T,
	run func(context.Context, *model.ProviderRecord, pipeline.Files) error,
) func(context.Context, *model.ProviderRecord, pipeline.Files) error {
	filter := liveFilter()
	return func(ctx context.Context, record *model.ProviderRecord, files pipeline.Files) error {
		if filter != "" && record.BindingKey != filter {
			t.Logf("  sweep reached %s -- outside MANDI_LIVE_BINDING_KEY, skipped", record.BindingKey)
			return nil
		}
		t.Logf("  ▶ %s -> %s", record.BindingKey, files.RegistryPath)
		return run(ctx, record, files)
	}
}

// liveRegistry connects to the registry the live tests read.
func liveRegistry(ctx context.Context, t *testing.T, registryURL string) *sunbirdRegistry.Client {
	t.Helper()
	registry, closeRegistry, err := sunbirdRegistry.New(ctx, nil, &sunbirdRegistry.Config{
		URL: registryURL, Entity: "Participant", ProviderEntity: "ProviderSchema", Timeout: 10,
	})
	if err != nil {
		t.Fatalf("sunbirdRegistry.New: %v", err)
	}
	t.Cleanup(func() { _ = closeRegistry() })
	return registry
}

// sanctioned is what a sweep will run: every binding with a publish action
// whose YAML this binary embeds, narrowed by the filter. Resolved up front so
// a test knows what to expect and can clear each one's run log.
func sanctioned(ctx context.Context, t *testing.T, registry *sunbirdRegistry.Client) []*model.ProviderRecord {
	t.Helper()
	keys, err := registry.ProviderBindingKeys(ctx)
	if err != nil {
		t.Fatalf("listing the registry: %v", err)
	}
	filter := liveFilter()
	var out []*model.ProviderRecord
	for _, key := range keys {
		if filter != "" && key != filter {
			continue
		}
		record, err := registry.ProviderRecord(ctx, key)
		if err != nil {
			continue
		}
		path, err := pipeline.PipelinePathFor(record)
		if err != nil {
			continue
		}
		if _, err := implementation.PublishPipeline(path); err != nil {
			t.Logf("  %s names %s, which this binary does not embed -- the sweep will skip it", key, path)
			continue
		}
		out = append(out, record)
	}
	if len(out) == 0 {
		t.Skip("the registry sanctions no pipeline this binary carries (after MANDI_LIVE_BINDING_KEY)")
	}
	for _, record := range out {
		t.Logf("  will run: %s (%s)", record.BindingKey, record.CapabilityCode)
	}
	return out
}

// clearRunLog gives every sanctioned capability a clean slate, so the first
// sweep is genuinely the first of its cron firing rather than inheriting an
// earlier experiment's row.
func clearRunLog(ctx context.Context, t *testing.T, runLog pipeline.RunLog, records []*model.ProviderRecord) {
	t.Helper()
	for _, record := range records {
		if err := runLog.RecordPipelineRun(ctx, record.CapabilityCode, time.Time{}); err != nil {
			t.Fatalf("clearing the run log for %s: %v", record.CapabilityCode, err)
		}
	}
}

// liveRun is one pipeline's outcome within one sweep.
type liveRun struct {
	bindingKey string
	report     pipeline.RunReport
	err        error
}

// sweepRecorder collects each sweep's runs, keyed by sweep number.
type sweepRecorder struct {
	mu     sync.Mutex
	sweeps [][]liveRun
}

func (s *sweepRecorder) begin() {
	s.mu.Lock()
	s.sweeps = append(s.sweeps, nil)
	s.mu.Unlock()
}

func (s *sweepRecorder) add(run liveRun) {
	s.mu.Lock()
	s.sweeps[len(s.sweeps)-1] = append(s.sweeps[len(s.sweeps)-1], run)
	s.mu.Unlock()
}

func (s *sweepRecorder) snapshot() [][]liveRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]liveRun(nil), s.sweeps...)
}

// logRun prints one pipeline's outcome the way an operator reads it.
func logRun(t *testing.T, run liveRun) {
	t.Helper()
	r := run.report
	published := 0
	if r.Published != nil {
		published = len(r.Published.Outcomes)
	}
	t.Logf("    %-45s due=%-5v built=%-3d published=%-3d %s",
		run.bindingKey, r.Due, len(r.Catalogues), published, r.Reason)
	if run.err != nil {
		t.Logf("    %-45s ERROR %v", "", run.err)
	}
	if r.Published != nil {
		for _, outcome := range r.Published.Outcomes {
			t.Logf("    %-45s   %-36s %s %s", "", outcome.CatalogID, outcome.Status, outcome.Reason)
		}
	}
}

// TestLive_CrawlerTick is one sweep, stopped at the schedule: every sanctioned
// pipeline is resolved, loaded and asked "are you due" -- and nothing is
// fetched.
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
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	t.Log("STEP 1 — read the crawler's config")
	cfg, err := publishConfigFrom(map[string]string{
		cfgPublishPipelines: "true",
		// Publishing stays off. This test reads and decides; it never sends.
		cfgPublishEnabled: "false",
	})
	if err != nil {
		t.Fatalf("config refused: %v", err)
	}
	t.Logf("  enabled=%v tick=%s publish=%v filter=%q", cfg.enabled, cfg.tick, cfg.publish, liveFilter())
	t.Logf("  this binary carries pipelines: %v", implementation.PublishPipelines())

	t.Log("STEP 2 — connect to the registry and see what it sanctions")
	registry := liveRegistry(ctx, t, registryURL)
	expected := sanctioned(ctx, t, registry)

	t.Log("STEP 3 — build the crawler's publish sweep")
	src, err := buildPublishSource(registry, log)
	if err != nil {
		t.Fatalf("buildPublishSource: %v", err)
	}
	runner := newPublishSweep(cfg, src, nil, log)

	// The sweep's real work is swapped for a DRY RUN: everything up to and
	// including "is it due" happens for real -- the listing, each lookup, the
	// publish-action gate, the pipeline load, the cron decision -- and nothing
	// is fetched.
	rec := &sweepRecorder{}
	runner.run = watched(t, func(ctx context.Context, record *model.ProviderRecord, files pipeline.Files) error {
		report, err := pipeline.Run(ctx, pipeline.RunOptions{
			Pipeline: files,
			Record:   record,
			Now:      time.Now(),
			DryRun:   true, // stop here: decide, do not fetch
			Log:      log,
		})
		rec.add(liveRun{record.BindingKey, report, err})
		return err
	})

	t.Log("STEP 4 — tick")
	rec.begin()
	runner.tick(ctx)

	t.Log("STEP 5 — the decisions")
	runs := rec.snapshot()[0]
	for _, run := range runs {
		logRun(t, run)
		if run.err != nil {
			t.Errorf("%s: %v", run.bindingKey, run.err)
		}
		if run.report.PipelinePath == "" {
			t.Errorf("%s reached no pipeline", run.bindingKey)
		}
		if len(run.report.Catalogues) != 0 {
			t.Errorf("%s: a dry run produced %d catalogues", run.bindingKey, len(run.report.Catalogues))
		}
	}
	if len(runs) != len(expected) {
		t.Errorf("the sweep ran %d pipelines, the registry sanctions %d", len(runs), len(expected))
	}
}

// TestLive_CrawlerBuildsCatalogues is the same sweep, allowed to finish.
//
// Every sanctioned pipeline runs for real against its upstream (if it has
// one) and writes its catalogues to disk. Publishing stays OFF, so nothing
// leaves this machine. Slow by nature: mandi's per-state calls are sequential.
//
//	MANDI_LIVE=1 SUNBIRD_REGISTRY_URL=http://localhost:8081/api/v1 \
//	  MANDI_LIVE_OUT=/tmp/catalogs \
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

	cfg, err := publishConfigFrom(map[string]string{
		cfgPublishPipelines: "true",
		// OFF. This test reads from the upstreams and writes to disk.
		cfgPublishEnabled:          "false",
		cfgPublishCatalogOutputDir: outDir,
	})
	if err != nil {
		t.Fatalf("config refused: %v", err)
	}

	registry := liveRegistry(ctx, t, registryURL)
	expected := sanctioned(ctx, t, registry)

	src, err := buildPublishSource(registry, log)
	if err != nil {
		t.Fatalf("buildPublishSource: %v", err)
	}
	runner := newPublishSweep(cfg, src, nil, log)
	rec := &sweepRecorder{}
	runner.run = watched(t, func(ctx context.Context, record *model.ProviderRecord, files pipeline.Files) error {
		report, err := pipeline.Run(ctx, pipeline.RunOptions{
			Pipeline:  files,
			Record:    record,
			OutDir:    cfg.outDir,
			Publish:   cfg.publish, // false
			Publisher: runner.publisher,
			Now:       time.Now(),
			Log:       log,
		})
		rec.add(liveRun{record.BindingKey, report, err})
		return err
	})

	started := time.Now()
	t.Logf("sweep starting; this takes minutes")
	rec.begin()
	runner.tick(ctx)
	t.Logf("sweep took %s", time.Since(started).Round(time.Second))

	runs := rec.snapshot()[0]
	if len(runs) != len(expected) {
		t.Errorf("the sweep ran %d pipelines, the registry sanctions %d", len(runs), len(expected))
	}
	for _, run := range runs {
		logRun(t, run)
		r := run.report
		if run.err != nil {
			t.Errorf("%s failed: %v", run.bindingKey, run.err)
			continue
		}
		if !r.Due {
			t.Logf("    %s not due, so nothing ran", run.bindingKey)
			continue
		}
		names := make([]string, 0, len(r.Counters))
		for name := range r.Counters {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			t.Logf("      %-38s %d", name, r.Counters[name])
		}
		t.Logf("      written to %s", r.OutDir)
		if len(r.Catalogues) == 0 {
			t.Errorf("%s produced no catalogues at all", run.bindingKey)
		}
		if r.Errors > 0 {
			t.Errorf("%s: %d parts of the collection failed; a real run would refuse to publish", run.bindingKey, r.Errors)
		}
		if r.Published != nil {
			t.Errorf("%s: publishing was off, but a publish result came back", run.bindingKey)
		}
	}
}

// TestLive_CrawlerPublishesThenDeclines is the whole thing, twice.
//
// Sweep 1 runs every sanctioned pipeline for real and PUTS CATALOGUES ON THE
// NETWORK. Sweep 2 fires immediately afterwards and every pipeline must
// decline, because the run log -- real rows in Postgres, written by sweep 1 --
// says this firing has already been served. That second sweep is the point:
// without it, a crawler restarting at 00:05 would republish everything.
//
// THIS ONE PUBLISHES. It needs its own opt-in for that reason, and a database,
// because a run log that does not outlive the process proves nothing:
//
//	MANDI_LIVE=1 MANDI_LIVE_PUBLISH=1 \
//	  SUNBIRD_REGISTRY_URL=http://localhost:8081/api/v1 \
//	  CATALOGCRAWLER_DSN='postgres://mandi:mandi@localhost:55432/catalogcrawler?sslmode=disable' \
//	  MANDI_FROM_DATE=23-06-2026 MANDI_TO_DATE=23-09-2026 \
//	  CATALOG_PUBLISH_URL=http://localhost:9200 \
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

	outDir := os.Getenv("MANDI_LIVE_OUT")
	if outDir == "" {
		outDir = t.TempDir()
	}
	cfg, err := publishConfigFrom(map[string]string{
		cfgPublishPipelines:        "true",
		cfgPublishEnabled:          "true", // <- the network
		cfgPublishCatalogOutputDir: outDir,
	})
	if err != nil {
		t.Fatalf("config refused: %v", err)
	}

	registry := liveRegistry(ctx, t, registryURL)
	expected := sanctioned(ctx, t, registry)

	src, err := buildPublishSource(registry, log)
	if err != nil {
		t.Fatalf("buildPublishSource: %v", err)
	}
	runner := newPublishSweep(cfg, src, runLog, log)
	rec := &sweepRecorder{}
	runner.run = watched(t, func(ctx context.Context, record *model.ProviderRecord, files pipeline.Files) error {
		report, err := pipeline.Run(ctx, pipeline.RunOptions{
			Pipeline:  files,
			Record:    record,
			RunLog:    runLog,
			OutDir:    cfg.outDir,
			Publish:   cfg.publish,
			Publisher: runner.publisher,
			Now:       time.Now(),
			Log:       log,
		})
		rec.add(liveRun{record.BindingKey, report, err})
		return err
	})

	clearRunLog(ctx, t, runLog, expected)

	t.Log("================ SWEEP 1 — every pipeline should run and publish ================")
	started := time.Now()
	rec.begin()
	runner.tick(ctx)
	t.Logf("sweep 1 took %s", time.Since(started).Round(time.Second))

	t.Log("================ SWEEP 2 — every pipeline should decline ================")
	rec.begin()
	runner.tick(ctx)

	sweeps := rec.snapshot()
	for i, runs := range sweeps {
		t.Logf("sweep %d:", i+1)
		for _, run := range runs {
			logRun(t, run)
		}
		if len(runs) != len(expected) {
			t.Errorf("sweep %d ran %d pipelines, the registry sanctions %d", i+1, len(runs), len(expected))
		}
	}

	for _, run := range sweeps[0] {
		r := run.report
		switch {
		case run.err != nil:
			t.Errorf("sweep 1, %s failed: %v", run.bindingKey, run.err)
		case !r.Due:
			t.Errorf("sweep 1, %s was not due: %s", run.bindingKey, r.Reason)
		case r.Published == nil:
			t.Errorf("sweep 1, %s: publishing was on, but no publish result came back", run.bindingKey)
		case r.Published.HasFailures():
			t.Errorf("sweep 1, %s: at least one catalogue did not reach the network intact "+
				"(PARTIAL counts: the catalogue indexed with resources missing)", run.bindingKey)
		}
	}
	for _, run := range sweeps[1] {
		if run.err != nil {
			t.Errorf("sweep 2, %s failed: %v", run.bindingKey, run.err)
		}
		if run.report.Due {
			t.Errorf("sweep 2, %s ran again; a restart would republish the whole day", run.bindingKey)
		}
		if run.report.Published != nil {
			t.Errorf("sweep 2, %s published; the run log did not stop it", run.bindingKey)
		}
	}
}

// idleSource discovers nothing, so the crawler's own index-poll and
// catalog-sync loops run for real and find no work. The publish sweep is what
// this test watches; the crawl loops are present because the Scheduler really
// does run them alongside it, and pretending otherwise would test a shape
// production does not have.
type idleSource struct{}

func (idleSource) Discover(context.Context) ([]crawlmanager.IndexRef, error) { return nil, nil }

// TestLive_CrawlerTicksOnItsOwnClock starts the REAL Scheduler and watches it.
//
// Nothing here calls tick() by hand. The Scheduler's own ticker fires the
// sweep: once immediately, then every interval. The first sweep runs every
// sanctioned pipeline and publishes; every later sweep must find each one's
// run log already satisfied for its cron firing and stand down. What you are
// watching is the wait.
//
// A firing is one WHOLE SWEEP: the count only advances after every pipeline in
// it has finished, so the test never stops mid-sweep and cancels a lookup.
//
//	MANDI_LIVE=1 MANDI_LIVE_PUBLISH=1 \
//	  SUNBIRD_REGISTRY_URL=http://localhost:8081/api/v1 \
//	  CATALOGCRAWLER_DSN='postgres://mandi:mandi@localhost:55432/catalogcrawler?sslmode=disable' \
//	  MANDI_FROM_DATE=23-06-2026 MANDI_TO_DATE=23-09-2026 \
//	  CATALOG_PUBLISH_URL=http://localhost:9200 \
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
	wantSweeps := 3
	if raw := os.Getenv("MANDI_TICKS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 2 {
			t.Fatalf("MANDI_TICKS=%q must be at least 2 (one run, one stand-down)", raw)
		}
		wantSweeps = n
	}

	// Long enough for the first sweep plus the remaining waits, with slack.
	budget := time.Duration(wantSweeps)*interval + 8*time.Minute
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

	outDir := os.Getenv("MANDI_LIVE_OUT")
	if outDir == "" {
		outDir = t.TempDir()
	}
	cfg, err := publishConfigFrom(map[string]string{
		cfgPublishPipelines:        "true",
		cfgPublishEnabled:          map[bool]string{true: "true", false: "false"}[publishing],
		cfgPublishCatalogOutputDir: outDir,
		cfgPublishTickIntervalSec:  strconv.Itoa(int(interval.Seconds())),
	})
	if err != nil {
		t.Fatalf("config refused: %v", err)
	}

	registry := liveRegistry(ctx, t, registryURL)
	expected := sanctioned(ctx, t, registry)

	src, err := buildPublishSource(registry, log)
	if err != nil {
		t.Fatalf("buildPublishSource: %v", err)
	}
	runner := newPublishSweep(cfg, src, st, log)
	rec := &sweepRecorder{}
	runner.run = watched(t, func(ctx context.Context, record *model.ProviderRecord, files pipeline.Files) error {
		report, err := pipeline.Run(ctx, pipeline.RunOptions{
			Pipeline:  files,
			Record:    record,
			RunLog:    st,
			OutDir:    cfg.outDir,
			Publish:   cfg.publish,
			Publisher: runner.publisher,
			Now:       time.Now(),
			Log:       log,
		})
		rec.add(liveRun{record.BindingKey, report, err})
		return err
	})

	clearRunLog(ctx, t, st, expected)

	// Each firing of the scheduler is one whole sweep. `done` closes only
	// after the sweep that reaches wantSweeps has finished every pipeline.
	var mu sync.Mutex
	var firedAt []time.Time
	done := make(chan struct{})
	sweep := func(ctx context.Context) {
		// The scheduler keeps firing until Stop; sweeps past the ones being
		// watched are not recorded, so the summary reads a settled set.
		mu.Lock()
		finished := len(firedAt) >= wantSweeps
		mu.Unlock()
		if finished {
			return
		}
		at := time.Now()
		rec.begin()
		runner.tick(ctx)
		mu.Lock()
		firedAt = append(firedAt, at)
		n := len(firedAt)
		mu.Unlock()
		t.Logf("──────── SWEEP %d at %s (took %s) ────────", n, at.Format("15:04:05"),
			time.Since(at).Round(time.Second))
		for _, run := range rec.snapshot()[n-1] {
			logRun(t, run)
		}
		if n == wantSweeps {
			close(done)
		}
	}

	// The real Scheduler, with the crawl loops it always carries. Their
	// cadences are pushed out of the way so the output is the publish sweep's;
	// they still run once at startup, as they do in production.
	sched := NewScheduler(crawlmanager.Params{Store: st, Source: idleSource{}, Log: log},
		SchedulerConfig{
			IndexInterval:     24 * time.Hour,
			CatalogInterval:   24 * time.Hour,
			ParkSweepInterval: 24 * time.Hour,
		}, log)
	if err := sched.AddPeriodic(cfg.tick, sweep); err != nil {
		t.Fatalf("AddPeriodic: %v", err)
	}

	t.Logf("starting the scheduler: sweep every %s, watching for %d sweeps", cfg.tick, wantSweeps)
	t.Logf("publishing is %s", map[bool]string{true: "ON — catalogues will reach the network", false: "OFF"}[publishing])
	started := time.Now()
	sched.Start(ctx)
	defer sched.Stop()

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("only %d sweeps in %s; wanted %d", len(rec.snapshot()), time.Since(started).Round(time.Second), wantSweeps)
	}

	mu.Lock()
	defer mu.Unlock()
	sweeps := rec.snapshot()[:wantSweeps]
	t.Log("════════ SUMMARY ════════")
	for i, runs := range sweeps {
		gap := ""
		if i > 0 {
			gap = " (+" + firedAt[i].Sub(firedAt[i-1]).Round(time.Second).String() + " after the previous)"
		}
		t.Logf("  sweep %d  %s%s", i+1, firedAt[i].Format("15:04:05"), gap)
		for _, run := range runs {
			logRun(t, run)
		}
		if len(runs) != len(expected) {
			t.Errorf("sweep %d ran %d pipelines, the registry sanctions %d", i+1, len(runs), len(expected))
		}
	}

	// Sweep 1 does the work, for every pipeline.
	for _, run := range sweeps[0] {
		if run.err != nil {
			t.Errorf("sweep 1, %s failed: %v", run.bindingKey, run.err)
			continue
		}
		if !run.report.Due {
			t.Errorf("sweep 1, %s was not due: %s", run.bindingKey, run.report.Reason)
		}
		if len(run.report.Catalogues) == 0 {
			t.Errorf("sweep 1, %s built nothing", run.bindingKey)
		}
	}

	// Every later sweep stands down, for every pipeline, because each run log
	// says its cron firing has been served. This is what stops a restart
	// republishing.
	for i, runs := range sweeps[1:] {
		for _, run := range runs {
			if run.report.Due {
				t.Errorf("sweep %d, %s ran again; a restart would republish the whole day: %s",
					i+2, run.bindingKey, run.report.Reason)
			}
			if len(run.report.Catalogues) != 0 {
				t.Errorf("sweep %d, %s built %d catalogues; it should have fetched nothing",
					i+2, run.bindingKey, len(run.report.Catalogues))
			}
		}
	}

	// And the waits were real, not a burst.
	for i := 1; i < wantSweeps; i++ {
		if gap := firedAt[i].Sub(firedAt[i-1]); gap < interval/2 {
			t.Errorf("sweep %d came %s after the previous, but the interval is %s", i+1, gap, interval)
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

	t.Logf("the registry holds %d capabilities; this binary carries pipelines %v",
		len(keys), implementation.PublishPipelines())

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

		if _, err := implementation.PublishPipeline(path); err != nil {
			unserveable++
			t.Logf("    gate           NO PIPELINE — the registry sanctions publishing, but")
			t.Logf("                   %v", err)
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
