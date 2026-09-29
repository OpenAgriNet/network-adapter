package catalogcrawler

// publishsweep_test.go covers the crawler's side of the scheduled publish
// pipelines: reading its configuration, sweeping the registry, skipping what
// does not publish, and not letting two sweeps overlap. The pipeline's own
// behaviour -- the registry gate, the cron schedule, the fetch/build/publish --
// is tested in catalogpublisher/pipeline and pkg/plugin.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogcrawler/internal/sink"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogpublisher/pipeline"

	"github.com/beckn/catalog-core/pkg/catalog/crawler"
)

func TestPublishConfigIsOffUnlessAskedFor(t *testing.T) {
	cfg, err := publishConfigFrom(map[string]string{"dbDsn": "postgres://x"})
	if err != nil {
		t.Fatalf("publishConfigFrom: %v", err)
	}
	if cfg.enabled {
		t.Error("the publish sweep is on without being configured")
	}
}

func TestPublishConfigReadsItsSettings(t *testing.T) {
	cfg, err := publishConfigFrom(map[string]string{
		cfgPublishPipelines:        "true",
		cfgPublishEnabled:          "true",
		cfgPublishTickIntervalSec:  "60",
		cfgPublishCatalogOutputDir: "/tmp/catalogs",
		cfgDiscoveryURL:            "http://provider-adapter:9200/publish",
	})
	if err != nil {
		t.Fatalf("publishConfigFrom: %v", err)
	}
	if !cfg.enabled {
		t.Fatal("publishPipelines: true did not enable the sweep")
	}
	if !cfg.publish {
		t.Error("publish was configured true but read as false")
	}
	if cfg.tick != time.Minute {
		t.Errorf("tick = %s, want 1m", cfg.tick)
	}
	if cfg.outDir != "/tmp/catalogs" {
		t.Errorf("outDir = %q", cfg.outDir)
	}
	// The crawler's one publish address reaches every pipeline.
	// The pipelines' base is discoveryPushUrl without its /publish, so they
	// reach the same endpoint the sink publishes crawled catalogs to.
	if cfg.publishURL != "http://provider-adapter:9200" {
		t.Errorf("publishURL = %q, want discoveryPushUrl's base", cfg.publishURL)
	}
}

// Publishing defaults to off even when the sweep is enabled: turning the
// schedule on and reaching the network are two decisions, and only one of them
// is visible to other people.
func TestPublishConfigPublishDefaultsToOff(t *testing.T) {
	cfg, err := publishConfigFrom(map[string]string{cfgPublishPipelines: "true"})
	if err != nil {
		t.Fatalf("publishConfigFrom: %v", err)
	}
	if !cfg.enabled {
		t.Fatal("not enabled")
	}
	if cfg.publish {
		t.Error("publishing is on by default")
	}
	if cfg.tick != defaultPublishTickInterval {
		t.Errorf("tick = %s, want the default %s", cfg.tick, defaultPublishTickInterval)
	}
}

// The retired key is REFUSED, not ignored: a deployment that used it to limit
// what publishes would otherwise start publishing everything the registry
// sanctions, and find out from the network.
func TestPublishConfigRefusesTheRetiredBindingKeyList(t *testing.T) {
	_, err := publishConfigFrom(map[string]string{
		cfgPublishBindingKeys: "agmarknet-live|openagrinet:MandiPrice",
		cfgPublishPipelines:   "true",
	})
	if err == nil {
		t.Fatal("the retired publishBindingKeys was accepted")
	}
	if !strings.Contains(err.Error(), cfgPublishPipelines) {
		t.Errorf("error %q does not say what to use instead", err)
	}
}

// stubRegistry stands in for the registry plugin: a list of keys, and one
// record (or error) per key.
type stubRegistry struct {
	mu      sync.Mutex
	keys    []string
	listErr error
	records map[string]*model.ProviderRecord
	errs    map[string]error
	lookups []string
}

func (s *stubRegistry) ProviderBindingKeys(context.Context) ([]string, error) {
	return s.keys, s.listErr
}

func (s *stubRegistry) ProviderRecord(_ context.Context, key string) (*model.ProviderRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lookups = append(s.lookups, key)
	if err := s.errs[key]; err != nil {
		return nil, err
	}
	if record, ok := s.records[key]; ok {
		return record, nil
	}
	return nil, definition.ErrProviderRecordNotFound
}

func publishingRecord(key, path string) *model.ProviderRecord {
	return &model.ProviderRecord{
		BindingKey: key,
		Actions: map[string]model.ActionPlan{
			"select":  {Method: "GET", Path: "/x"},
			"publish": {Mappings: path},
		},
	}
}

// ranSweep is a sweep over the REAL publishDiscoverer -- only the registry,
// the embed lookup and the run are stubs -- whose run records what it was
// given. Discovery and the sweep are tested together, as a tick runs them.
func ranSweep(reg *stubRegistry, resolveErr map[string]error) (*publishSweep, *[]string) {
	var ran []string
	var mu sync.Mutex
	log := slog.New(slog.DiscardHandler)
	sweep := &publishSweep{
		cfg: publishConfig{enabled: true},
		source: &publishDiscoverer{
			lookup: reg,
			log:    log,
			resolve: func(path string) (pipeline.Files, error) {
				if err := resolveErr[path]; err != nil {
					return pipeline.Files{}, err
				}
				return pipeline.Files{URL: path}, nil
			},
		},
		log: log,
		run: func(_ context.Context, record *model.ProviderRecord, files pipeline.Files) error {
			mu.Lock()
			defer mu.Unlock()
			ran = append(ran, record.BindingKey+" -> "+files.URL)
			return nil
		},
	}
	return sweep, &ran
}

// The whole point: the registry is the list. Every binding with a publish
// action runs; select-only and unusable bindings are skipped quietly; one
// capability's failure does not stop the next.
func TestPublishSweepRunsEveryPipelineTheRegistrySanctions(t *testing.T) {
	const (
		mandi   = "agmarknet-live|openagrinet:MandiPrice"
		weather = "mausamgram|openagrinet:WeatherObservation"
		advice  = "bharat-vistaar|openagrinet:KnowledgeAdvisory"
		broken  = "down|example:Broken"
		missing = "gone|example:NotEmbedded"
		retired = "old|example:Inactive"
	)
	reg := &stubRegistry{
		keys: []string{mandi, broken, advice, retired, missing, weather},
		records: map[string]*model.ProviderRecord{
			mandi:   publishingRecord(mandi, "pkg/mandi.yaml"),
			weather: publishingRecord(weather, "pkg/weather.yaml"),
			missing: publishingRecord(missing, "pkg/not-built-in.yaml"),
			advice: {BindingKey: advice, Actions: map[string]model.ActionPlan{
				"select": {Method: "GET", Path: "/x"},
			}},
		},
		errs: map[string]error{broken: errors.New("registry timeout")},
	}
	sweep, ran := ranSweep(reg, map[string]error{"pkg/not-built-in.yaml": errors.New("not embedded")})

	sweep.tick(context.Background())

	want := []string{mandi + " -> pkg/mandi.yaml", weather + " -> pkg/weather.yaml"}
	if strings.Join(*ran, "\n") != strings.Join(want, "\n") {
		t.Errorf("ran:\n%s\nwant:\n%s", strings.Join(*ran, "\n"), strings.Join(want, "\n"))
	}
	if len(reg.lookups) != len(reg.keys) {
		t.Errorf("looked up %v, want every listed key", reg.lookups)
	}
}

// A registry that cannot list must leave the tick as a no-op, not crash the
// crawler: the index-poll and catalog-sync loops are unrelated work that has
// to keep running.
func TestPublishSweepSurvivesARegistryFailure(t *testing.T) {
	reg := &stubRegistry{listErr: errors.New("registry unreachable")}
	sweep, ran := ranSweep(reg, nil)
	sweep.tick(context.Background()) // must not panic
	if len(*ran) != 0 || len(reg.lookups) != 0 {
		t.Errorf("ran %v, looked up %v after a failed list; want nothing", *ran, reg.lookups)
	}
}

// A registry plugin that cannot list is a startup error once the sweep is on.
func TestBuildPublishSourceRefusesARegistryThatCannotList(t *testing.T) {
	_, err := buildPublishSource(lookupOnly{}, slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("a registry that cannot list bindings was accepted")
	}
}

// Discovery is the one place a listing failure surfaces as an error; the
// sweep turns it into a quiet tick (TestPublishSweepSurvivesARegistryFailure).
func TestPublishDiscovererReportsAFailedListing(t *testing.T) {
	d := &publishDiscoverer{lookup: &stubRegistry{listErr: errors.New("down")}, log: slog.New(slog.DiscardHandler)}
	if _, err := d.Discover(context.Background()); err == nil {
		t.Fatal("a failed listing was reported as an empty registry")
	}
}

type lookupOnly struct{}

func (lookupOnly) Lookup(context.Context, *model.Subscription) ([]model.Subscription, error) {
	return nil, nil
}

// A sweep takes minutes; the tick is far shorter. A second sweep starting
// while the first is still running would double every upstream call and race
// to write the same catalog files.
func TestPublishSweepDoesNotOverlap(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	var runs int
	var mu sync.Mutex

	reg := &stubRegistry{
		keys:    []string{"a|b"},
		records: map[string]*model.ProviderRecord{"a|b": publishingRecord("a|b", "p.yaml")},
	}
	sweep, _ := ranSweep(reg, nil)
	sweep.run = func(context.Context, *model.ProviderRecord, pipeline.Files) error {
		mu.Lock()
		runs++
		if runs == 1 {
			close(started)
		}
		mu.Unlock()
		<-release
		return nil
	}

	go sweep.tick(context.Background())
	<-started

	sweep.tick(context.Background()) // must return immediately, not block or run

	mu.Lock()
	got := runs
	mu.Unlock()
	if got != 1 {
		t.Errorf("%d runs overlapped; want the second tick to have been skipped", got)
	}
	close(release)
}

// After a sweep finishes the next tick must be free to run again -- a guard
// that never releases silently stops the daily publish forever.
func TestPublishSweepRunsAgainAfterAFailedRun(t *testing.T) {
	reg := &stubRegistry{
		keys:    []string{"a|b"},
		records: map[string]*model.ProviderRecord{"a|b": publishingRecord("a|b", "p.yaml")},
	}
	sweep, _ := ranSweep(reg, nil)
	var runs int
	sweep.run = func(context.Context, *model.ProviderRecord, pipeline.Files) error {
		runs++
		return errors.New("this run failed")
	}

	sweep.tick(context.Background())
	sweep.tick(context.Background())

	if runs != 2 {
		t.Errorf("runs = %d, want 2; the guard did not release after a failure", runs)
	}
}

// publishBase strips only a trailing /publish (and slashes), so a pipeline
// appending /publish lands on the same endpoint the sink uses.
func TestPublishBaseStripsOnlyTheTrailingPublish(t *testing.T) {
	for in, want := range map[string]string{
		"http://pa:9200/publish":   "http://pa:9200",
		"http://pa:9200/publish/":  "http://pa:9200",
		" http://pa:9200/publish ": "http://pa:9200",
		"http://pa:9200":           "http://pa:9200",
		"http://pa:9200/publisher": "http://pa:9200/publisher",
	} {
		if got := publishBase(in); got != want {
			t.Errorf("publishBase(%q) = %q, want %q", in, got, want)
		}
	}
}

// A publish action that names no pipeline is skipped, not run.
func TestPublishDiscovererSkipsAPublishActionWithNoPipeline(t *testing.T) {
	const key = "p|example:NoPath"
	reg := &stubRegistry{
		keys:    []string{key},
		records: map[string]*model.ProviderRecord{key: publishingRecord(key, "")},
	}
	sweep, ran := ranSweep(reg, nil)
	sweep.tick(context.Background())
	if len(*ran) != 0 {
		t.Errorf("ran %v, want nothing for a publish action with no mappings", *ran)
	}
}

// ranAt is a run log that says the pipeline last ran at a fixed time.
type ranAt struct{ last time.Time }

func (r ranAt) LastPipelineRun(context.Context, string) (time.Time, error) { return r.last, nil }
func (r ranAt) RecordPipelineRun(context.Context, string, time.Time) error { return nil }

// mandiPipelinePath is the Mandi pipeline's URL: its folder hosted on a
// loopback server, as production hosts it on https.
var mandiPipelinePath = func() string {
	base, _, err := pipeline.ServeMappings(os.DirFS("../MandiPrice/catalogpublish"), ".")
	if err != nil {
		panic(err)
	}
	return base + "/agmarknet.yaml"
}()

// newPublishSweep wires the real run and a publisher, so a tick does real work.
func TestNewPublishSweepWiresTheRunAndThePublisher(t *testing.T) {
	sweep := newPublishSweep(publishConfig{enabled: true}, &fixedTargets{}, nil, slog.New(slog.DiscardHandler))
	if sweep.run == nil || sweep.publisher == nil {
		t.Fatalf("sweep = %+v, want run and publisher set", sweep)
	}
}

// runPipeline hands the record to the real frame: a pipeline the run log says
// already ran is not due, and nothing is fetched.
func TestRunPipelineStandsDownWhenNotDue(t *testing.T) {
	files, err := pipeline.RemotePipeline(mandiPipelinePath)
	if err != nil {
		t.Fatalf("PublishPipeline: %v", err)
	}
	sweep := newPublishSweep(publishConfig{enabled: true}, &fixedTargets{}, ranAt{last: time.Now()}, slog.New(slog.DiscardHandler))
	record := publishingRecord("agmarknet-live|openagrinet:MandiPrice", mandiPipelinePath)

	if err := sweep.runPipeline(context.Background(), record, files); err != nil {
		t.Fatalf("runPipeline: %v", err)
	}
}

// A record naming a pipeline other than the files it is run with is refused
// by the frame's registry gate, and the error reaches the sweep.
func TestRunPipelineReportsTheFramesRefusal(t *testing.T) {
	files, err := pipeline.RemotePipeline(mandiPipelinePath)
	if err != nil {
		t.Fatalf("PublishPipeline: %v", err)
	}
	sweep := newPublishSweep(publishConfig{enabled: true}, &fixedTargets{}, nil, slog.New(slog.DiscardHandler))
	record := publishingRecord("x|openagrinet:MandiPrice", "pkg/plugin/implementation/Other/catalogpublish-x/p.yaml")

	if err := sweep.runPipeline(context.Background(), record, files); err == nil {
		t.Fatal("a record naming another pipeline was run")
	}
}

// fixedTargets is a discovery source with fixed targets.
type fixedTargets struct{ targets []publishTarget }

func (s *fixedTargets) Discover(context.Context) ([]publishTarget, error) { return s.targets, nil }

// A failure that will not clear must stop retrying.
//
// A failed run is deliberately not recorded, so the next tick tries again --
// which is right for a transient outage at midnight. But a permanent failure
// (a rejected catalog, a credential that will not work) then re-fetches and
// re-publishes EVERYTHING every five minutes: about 288 full runs a day, each
// re-sending catalogs the network already accepted.
//
// After a few attempts the firing is marked served, so the pipeline waits for
// its next scheduled firing instead.
func TestAPermanentFailureStopsRetryingWithinOneFiring(t *testing.T) {
	runLog := &countingRunLog{}
	attempts := 0

	sweep := &publishSweep{
		cfg:    publishConfig{},
		runLog: runLog,
		log:    slog.New(slog.DiscardHandler),
		source: staticSource{targets: []publishTarget{{
			record: &model.ProviderRecord{BindingKey: "who|example:Thing"},
			files:  pipeline.Files{URL: "https://host/x.yaml"},
		}}},
	}
	// The fake stands in for pipeline.Run, including its schedule gate: once
	// the firing has been marked served the real pipeline reports NOT DUE and
	// does no work, which is what the budget buys. Without modelling that, the
	// test would measure a situation production never reaches.
	sweep.run = func(ctx context.Context, record *model.ProviderRecord, files pipeline.Files) error {
		attempts++
		if runLog.recorded > 0 {
			return nil // not due: this firing has been served
		}
		return sweep.afterRun(ctx, "example:Thing", errAlwaysFails)
	}

	// Many more ticks than the cap.
	for i := 0; i < 20; i++ {
		sweep.tick(context.Background())
	}

	if attempts != 20 {
		t.Fatalf("the sweep called the pipeline %d times, want 20 (the guard is not about skipping ticks)", attempts)
	}
	if runLog.recorded == 0 {
		t.Error("a permanently failing pipeline never marked its firing served, so it retries every tick forever")
	}
	if runLog.recorded > 1 {
		t.Errorf("the firing was marked served %d times; once per exhausted firing is enough", runLog.recorded)
	}
	if attempts-maxAttemptsPerFiring < 10 {
		t.Errorf("only %d ticks stood down after the give-up; the budget bought nothing", attempts-maxAttemptsPerFiring)
	}
}

// The budget is per FIRING, not for the life of the process: a pipeline that
// exhausts Monday's attempts must get a full set on Tuesday.
func TestTheNextFiringGetsAFreshBudget(t *testing.T) {
	runLog := &countingRunLog{}
	sweep := &publishSweep{log: slog.New(slog.DiscardHandler), runLog: runLog}

	for i := 0; i < maxAttemptsPerFiring; i++ {
		_ = sweep.afterRun(context.Background(), "example:Thing", errAlwaysFails)
	}
	if runLog.recorded != 1 {
		t.Fatalf("the first firing was marked served %d times, want 1", runLog.recorded)
	}

	// The schedule comes round; the run is due again, and fails again. Only a
	// failure reaches here, because a not-due run never gets this far.
	for i := 0; i < maxAttemptsPerFiring; i++ {
		_ = sweep.afterRun(context.Background(), "example:Thing", errAlwaysFails)
	}
	if runLog.recorded != 2 {
		t.Errorf("the second firing was marked served %d times in total, want 2 -- "+
			"the budget did not reset, so the next day retries every tick", runLog.recorded)
	}
}

// A run that succeeds must clear the count, or a pipeline that fails twice on
// Monday has fewer attempts left on Tuesday.
func TestASuccessfulRunClearsTheFailureCount(t *testing.T) {
	sweep := &publishSweep{log: slog.New(slog.DiscardHandler), runLog: &countingRunLog{}}

	_ = sweep.afterRun(context.Background(), "example:Thing", errAlwaysFails)
	_ = sweep.afterRun(context.Background(), "example:Thing", nil)

	if budget, held := sweep.failures["example:Thing"]; held {
		t.Errorf("a success left %d failed attempts on the books", budget.count)
	}
}

// A cancelled or timed-out run is not a failure the pipeline caused -- it is
// this process shutting down, or a deadline this tick itself imposed.
// Counting it against the attempt budget can give up on a firing that never
// got a real try, and mark it served for the rest of the schedule window.
func TestAfterRunDoesNotBurnTheBudgetOnCancellation(t *testing.T) {
	sweep := &publishSweep{log: slog.New(slog.DiscardHandler), runLog: &countingRunLog{}}

	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		_ = sweep.afterRun(context.Background(), "example:Thing", fmt.Errorf("upstream call: %w", cause))
	}
	if budget, held := sweep.failures["example:Thing"]; held {
		t.Errorf("failures[%q] = %+v, want no budget entry for a cancelled/timed-out run", "example:Thing", budget)
	}
}

// Once the tick's own context has ended, running the rest of the targets is
// wasted work racing a process that is already shutting down -- and each of
// those calls would itself fail on the same dead context, adding more of the
// noise this same fix is removing from the log.
func TestTickStopsCallingTargetsOnceItsContextEnds(t *testing.T) {
	calls := 0
	sweep := &publishSweep{
		log: slog.New(slog.DiscardHandler),
		source: staticSource{targets: []publishTarget{
			{record: &model.ProviderRecord{BindingKey: "a|example:Thing"}, files: pipeline.Files{URL: "https://host/a.yaml"}},
			{record: &model.ProviderRecord{BindingKey: "b|example:Thing"}, files: pipeline.Files{URL: "https://host/b.yaml"}},
		}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	sweep.run = func(context.Context, *model.ProviderRecord, pipeline.Files) error {
		calls++
		cancel()
		return fmt.Errorf("upstream call: %w", context.Canceled)
	}

	sweep.tick(ctx)

	if calls != 1 {
		t.Errorf("the run was called %d time(s) after its context ended, want 1 (stop, don't keep going)", calls)
	}
}

// A permanent fault -- a pipeline file that will never load, an input type
// this pipeline does not implement, a non-https URL -- will not clear on
// retry. Spending the full attempt budget on it is 3 wasted fetches for an
// answer the first one already gave; it should give up on the first attempt.
func TestAPermanentFaultGivesUpOnTheFirstAttempt(t *testing.T) {
	runLog := &countingRunLog{}
	sweep := &publishSweep{log: slog.New(slog.DiscardHandler), runLog: runLog}

	permanentErr := crawler.PermanentFaultf(crawler.FaultClass("pipeline_spec"), "the pipeline file is not valid YAML")
	_ = sweep.afterRun(context.Background(), "example:Thing", permanentErr)

	if runLog.recorded != 1 {
		t.Fatalf("recorded %d time(s), want 1 (given up after the first attempt)", runLog.recorded)
	}
	if budget := sweep.failures["example:Thing"]; budget == nil || budget.count != 1 {
		t.Errorf("failures[%q] = %+v, want count 1", "example:Thing", budget)
	}
}

var errAlwaysFails = errors.New("this failure will not clear")

type countingRunLog struct {
	recorded int
	last     time.Time
	keys     []string
}

func (c *countingRunLog) LastPipelineRun(context.Context, string) (time.Time, error) {
	return c.last, nil
}

func (c *countingRunLog) RecordPipelineRun(_ context.Context, key string, at time.Time) error {
	c.keys = append(c.keys, key)
	c.recorded++
	c.last = at
	return nil
}

type staticSource struct{ targets []publishTarget }

func (s staticSource) Discover(context.Context) ([]publishTarget, error) { return s.targets, nil }

// Pipeline publishes carry this deployment's identity, from the same keys the
// crawl sink reads: the sweep's sink stamps them onto every body.
func TestPublishSweepCarriesTheDeploymentIdentity(t *testing.T) {
	cfg, err := publishConfigFrom(map[string]string{
		cfgPublishPipelines: "true",
		cfgParticipantID:    "bpp.example",
		cfgBppURI:           "https://bpp.example/bpp",
	})
	if err != nil {
		t.Fatalf("publishConfigFrom: %v", err)
	}
	sweep := newPublishSweep(cfg, &fixedTargets{}, nil, slog.New(slog.DiscardHandler))
	discovery, ok := sweep.publisher.(*sink.DiscoverySink)
	if !ok {
		t.Fatalf("publisher is %T, want *sink.DiscoverySink", sweep.publisher)
	}
	if discovery.ParticipantID != "bpp.example" || discovery.BppURI != "https://bpp.example/bpp" {
		t.Fatalf("sink identity = %q, %q; want the crawler's participantId and bppUri",
			discovery.ParticipantID, discovery.BppURI)
	}
}

// The sweep's publish calls log through the crawler's logger, not the
// process default, so they land with the rest of the crawler's lines.
func TestPublishSweepSinkLogsThroughTheCrawlerLogger(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	sweep := newPublishSweep(publishConfig{enabled: true}, &fixedTargets{}, nil, log)
	if got := sweep.publisher.(*sink.DiscoverySink).Client.Log; got != log {
		t.Fatalf("sink logger = %v, want the crawler's", got)
	}
}

// narrowingRegistry is a stubRegistry that can also say, from its listing,
// which bindings serve an action.
type narrowingRegistry struct {
	*stubRegistry
	serving map[string][]string
}

func (n narrowingRegistry) ProviderBindingKeysServing(_ context.Context, action string) ([]string, error) {
	return n.serving[action], nil
}

// A registry that can narrow its listing is asked to: only the bindings that
// publish are resolved, rather than every capability it holds.
func TestPublishDiscovererResolvesOnlyPublishingBindings(t *testing.T) {
	stub := &stubRegistry{
		keys: []string{"a|x:A", "b|x:B", "c|x:C"},
		records: map[string]*model.ProviderRecord{
			"b|x:B": publishingRecord("b|x:B", "pkg/plugin/implementation/X/catalogpublish-y/y.yaml"),
		},
	}
	d := &publishDiscoverer{
		lookup:  narrowingRegistry{stubRegistry: stub, serving: map[string][]string{"publish": {"b|x:B"}}},
		resolve: func(string) (pipeline.Files, error) { return pipeline.Files{}, nil },
		log:     slog.New(slog.DiscardHandler),
	}
	targets, err := d.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(targets) != 1 {
		t.Errorf("targets = %d, want 1", len(targets))
	}
	if fmt.Sprint(stub.lookups) != "[b|x:B]" {
		t.Errorf("resolved %v, want only the publishing binding", stub.lookups)
	}
}

// Giving up on a firing marks it served under the PIPELINE's key -- the same
// row the run itself reads -- or the next tick would not see the give-up and
// would start the storm again.
func TestGivingUpMarksThePipelineServed(t *testing.T) {
	files, err := pipeline.RemotePipeline(mandiPipelinePath)
	if err != nil {
		t.Fatalf("PublishPipeline: %v", err)
	}
	runLog := &countingRunLog{}
	sweep := newPublishSweep(publishConfig{enabled: true}, &fixedTargets{}, runLog, slog.New(slog.DiscardHandler))
	// A record naming some other pipeline: the frame's gate refuses it every
	// time, which is a failure that will not clear.
	record := publishingRecord("x|openagrinet:MandiPrice", "pkg/plugin/implementation/Other/catalogpublish-x/p.yaml")

	for i := 0; i < maxAttemptsPerFiring; i++ {
		if err := sweep.runPipeline(context.Background(), record, files); err == nil {
			t.Fatal("a refused pipeline reported success")
		}
	}
	if fmt.Sprint(runLog.keys) != "["+files.URL+"]" {
		t.Fatalf("give-up recorded under %v, want [%s]", runLog.keys, files.URL)
	}
}

// claimingElsewhere is a run log whose claim always loses: another replica
// owns every firing.
type claimingElsewhere struct{ countingRunLog }

func (c *claimingElsewhere) ClaimPipelineRun(context.Context, string, time.Time, time.Time) (bool, error) {
	return false, nil
}
func (c *claimingElsewhere) ReleasePipelineRun(context.Context, string, time.Time) error { return nil }

// A firing another replica claimed is neither a success nor a failure here:
// this replica did no work, so its attempt budget is left exactly as it was.
func TestRunPipelineLeavesTheBudgetWhenAnotherReplicaClaimed(t *testing.T) {
	files, err := pipeline.RemotePipeline(mandiPipelinePath)
	if err != nil {
		t.Fatalf("PublishPipeline: %v", err)
	}
	runLog := &claimingElsewhere{}
	sweep := newPublishSweep(publishConfig{enabled: true}, &fixedTargets{}, runLog, slog.New(slog.DiscardHandler))
	// Seeded under both keys, so the test holds whichever one the sweep uses.
	sweep.failures = map[string]*attemptBudget{files.URL: {count: 2}, "openagrinet:MandiPrice": {count: 2}}
	record := publishingRecord("agmarknet-live|openagrinet:MandiPrice", mandiPipelinePath)

	if err := sweep.runPipeline(context.Background(), record, files); err != nil {
		t.Fatalf("runPipeline: %v", err)
	}
	for _, key := range []string{files.URL, "openagrinet:MandiPrice"} {
		if budget := sweep.failures[key]; budget == nil || budget.count != 2 {
			t.Errorf("budget[%s] = %+v, want the 2 failed attempts untouched", key, budget)
		}
	}
	if runLog.recorded != 0 {
		t.Errorf("a run another replica owns was recorded %d times", runLog.recorded)
	}
}

// The crawler hands its own plugin config to every pipeline run, so a
// publish.<pipeline>.<key> line under plugins.crawler.config reaches the
// pipeline -- here a mistyped one, which the frame refuses by name.
func TestPublishSweepPassesPipelineOverridesFromPluginConfig(t *testing.T) {
	cfg, err := publishConfigFrom(map[string]string{
		cfgPublishPipelines:           "true",
		"publish.mandi-price.typoKey": "x",
	})
	if err != nil {
		t.Fatalf("publishConfigFrom: %v", err)
	}
	files, err := pipeline.RemotePipeline(mandiPipelinePath)
	if err != nil {
		t.Fatalf("PublishPipeline: %v", err)
	}
	sweep := newPublishSweep(cfg, &fixedTargets{}, nil, slog.New(slog.DiscardHandler))
	record := publishingRecord("agmarknet-live|openagrinet:MandiPrice", mandiPipelinePath)
	err = sweep.runPipeline(context.Background(), record, files)
	if err == nil || !strings.Contains(err.Error(), "typoKey") {
		t.Fatalf("err = %v; want the pipeline to see (and refuse) the configured override", err)
	}
}
