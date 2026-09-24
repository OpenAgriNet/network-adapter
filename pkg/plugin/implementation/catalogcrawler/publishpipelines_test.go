package catalogcrawler

// publishpipelines_test.go covers the crawler's side of the scheduled publish
// pipelines: reading its configuration, sweeping the registry, skipping what
// does not publish, and not letting two sweeps overlap. The pipeline's own
// behaviour -- the registry gate, the cron schedule, the fetch/build/publish --
// is tested in catalogpublisher/pipeline and pkg/plugin/implementation.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogpublisher/pipeline"
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

// ranSweep is a sweep whose resolve and run only record what they were given.
func ranSweep(reg *stubRegistry, resolveErr map[string]error) (*publishSweep, *[]string) {
	var ran []string
	var mu sync.Mutex
	sweep := &publishSweep{
		cfg:      publishConfig{enabled: true},
		registry: reg,
		log:      slog.New(slog.DiscardHandler),
		resolve: func(path string) (pipeline.Files, error) {
			if err := resolveErr[path]; err != nil {
				return pipeline.Files{}, err
			}
			return pipeline.Files{Path: path, RegistryPath: path}, nil
		},
		run: func(_ context.Context, record *model.ProviderRecord, files pipeline.Files) error {
			mu.Lock()
			defer mu.Unlock()
			ran = append(ran, record.BindingKey+" -> "+files.RegistryPath)
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
func TestNewPublishSweepRefusesARegistryThatCannotList(t *testing.T) {
	_, err := newPublishSweep(publishConfig{enabled: true}, lookupOnly{}, nil, slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("a registry that cannot list bindings was accepted")
	}
}

type lookupOnly struct{}

func (lookupOnly) Lookup(context.Context, *model.Subscription) ([]model.Subscription, error) {
	return nil, nil
}

// A sweep takes minutes; the tick is far shorter. A second sweep starting
// while the first is still running would double every upstream call and race
// to write the same catalogue files.
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
