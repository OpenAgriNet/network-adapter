package catalogcrawler

// mandi_test.go covers the crawler's side of the scheduled publish pipeline:
// reading its configuration, refusing a half-configured one, and not letting
// two runs overlap. The pipeline's own behaviour -- the registry gate, the
// cron schedule, the fetch/build/publish -- is tested in the agmarket package
// that owns it.

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
	agmarket "github.com/beckn-one/beckn-onix/pkg/plugin/implementation/MandiPrice/cataloguepublish-agmarket"
)

func TestPublishConfigIsOffUnlessAskedFor(t *testing.T) {
	cfg, err := publishConfigFrom(map[string]string{"dbDsn": "postgres://x"})
	if err != nil {
		t.Fatalf("publishConfigFrom: %v", err)
	}
	if cfg.enabled {
		t.Error("the publish pipeline is on without being configured")
	}
}

func TestPublishConfigReadsItsSettings(t *testing.T) {
	cfg, err := publishConfigFrom(map[string]string{
		cfgPublishBindingKeys:      "agmarknet-live|" + agmarket.Capability,
		cfgPublishEnabled:          "true",
		cfgPublishTickIntervalSec:  "60",
		cfgPublishCatalogOutputDir: "/tmp/mandi",
	})
	if err != nil {
		t.Fatalf("publishConfigFrom: %v", err)
	}
	if !cfg.enabled {
		t.Fatal("a configured binding key did not enable the pipeline")
	}
	if cfg.bindingKeys[0] != "agmarknet-live|"+agmarket.Capability {
		t.Errorf("bindingKey = %q", cfg.bindingKeys[0])
	}
	if !cfg.publish {
		t.Error("publish was configured true but read as false")
	}
	if cfg.tick != time.Minute {
		t.Errorf("tick = %s, want 1m", cfg.tick)
	}
	if cfg.outDir != "/tmp/mandi" {
		t.Errorf("outDir = %q", cfg.outDir)
	}
}

// Publishing defaults to off even when the pipeline is enabled: turning the
// schedule on and reaching the network are two decisions, and only one of them
// is visible to other people.
func TestPublishConfigPublishDefaultsToOff(t *testing.T) {
	cfg, err := publishConfigFrom(map[string]string{cfgPublishBindingKeys: "agmarknet-live|" + agmarket.Capability})
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

func TestPublishConfigRefusesAnUnusableBindingKey(t *testing.T) {
	for name, key := range map[string]string{
		"no separator":        "agmarknet-live",
		"no capability":       "agmarknet-live|",
		"no participant":      "|" + agmarket.Capability,
		"too many separators": "a|b|c",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := publishConfigFrom(map[string]string{cfgPublishBindingKeys: key}); err == nil {
				t.Errorf("binding key %q was accepted", key)
			}
		})
	}
}

// A capability this binary carries no pipeline for is a STARTUP error, not a
// tick that quietly does nothing every five minutes forever. The message has
// to name what is available, or an operator cannot tell a typo from a build
// that was never meant to publish it.
func TestPublishConfigRefusesACapabilityThisBinaryCannotServe(t *testing.T) {
	_, err := publishConfigFrom(map[string]string{
		cfgPublishBindingKeys: "mausamgram|openagrinet:WeatherObservation",
	})
	if err == nil {
		t.Fatal("a capability with no compiled-in pipeline was accepted")
	}
	if !strings.Contains(err.Error(), agmarket.Capability) {
		t.Errorf("error %q does not say which capabilities this binary can publish", err)
	}
}

// Several pipelines is the case the whole split exists for.
func TestPublishConfigReadsSeveralBindingKeys(t *testing.T) {
	cfg, err := publishConfigFrom(map[string]string{
		cfgPublishBindingKeys: "agmarknet-live|" + agmarket.Capability + " , agmarknet-test|" + agmarket.Capability,
	})
	if err != nil {
		t.Fatalf("publishConfigFrom: %v", err)
	}
	if len(cfg.bindingKeys) != 2 {
		t.Fatalf("bindingKeys = %v, want 2", cfg.bindingKeys)
	}
}

// stubRecordLookup stands in for the registry plugin.
type stubRecordLookup struct {
	mu     sync.Mutex
	calls  int
	record *model.ProviderRecord
	err    error
}

func (s *stubRecordLookup) ProviderRecord(context.Context, string) (*model.ProviderRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.record, s.err
}

func (s *stubRecordLookup) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// A registry that cannot be reached must leave the tick as a no-op, not crash
// the crawler: the index-poll and catalog-sync loops are unrelated work that
// has to keep running.
func TestPublishTickSurvivesARegistryFailure(t *testing.T) {
	lookup := &stubRecordLookup{err: errors.New("registry unreachable")}
	runner := &publishRunner{
		bindingKey: "exampleco|example:Thing",
		cfg:        publishConfig{enabled: true},
		lookup:     lookup,
		log:        slog.New(slog.DiscardHandler),
	}
	runner.tick(context.Background()) // must not panic
	if lookup.callCount() != 1 {
		t.Errorf("registry consulted %d times, want 1", lookup.callCount())
	}
}

// ErrProviderRecordNotFound is the registry answering "no", which is a normal
// state -- the capability may simply not be registered yet.
func TestPublishTickTreatsAnUnregisteredCapabilityAsQuiet(t *testing.T) {
	lookup := &stubRecordLookup{err: definition.ErrProviderRecordNotFound}
	runner := &publishRunner{
		bindingKey: "exampleco|example:Thing",
		cfg:        publishConfig{enabled: true},
		lookup:     lookup,
		log:        slog.New(slog.DiscardHandler),
	}
	runner.tick(context.Background())
}

// A collection run takes minutes; the tick is far shorter. A second run
// starting while the first is still fetching would double every upstream call
// and race to write the same catalogue files.
func TestPublishTickDoesNotOverlapRuns(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	var runs int
	var mu sync.Mutex

	runner := &publishRunner{
		bindingKey: "exampleco|example:Thing",
		cfg:        publishConfig{enabled: true},
		lookup:     &stubRecordLookup{record: &model.ProviderRecord{BindingKey: "a|b"}},
		log:        slog.New(slog.DiscardHandler),
		run: func(context.Context, *model.ProviderRecord) error {
			mu.Lock()
			runs++
			if runs == 1 {
				close(started)
			}
			mu.Unlock()
			<-release
			return nil
		},
	}

	go runner.tick(context.Background())
	<-started // the first run is now inside run and holding

	runner.tick(context.Background()) // must return immediately, not block or run

	mu.Lock()
	got := runs
	mu.Unlock()
	if got != 1 {
		t.Errorf("%d runs overlapped; want the second tick to have been skipped", got)
	}
	close(release)
}

// After a run finishes the next tick must be free to run again -- a guard that
// never releases silently stops the daily publish forever.
func TestPublishTickRunsAgainAfterARunFinishes(t *testing.T) {
	var runs int
	var mu sync.Mutex
	runner := &publishRunner{
		bindingKey: "exampleco|example:Thing",
		cfg:        publishConfig{enabled: true},
		lookup:     &stubRecordLookup{record: &model.ProviderRecord{BindingKey: "a|b"}},
		log:        slog.New(slog.DiscardHandler),
		run: func(context.Context, *model.ProviderRecord) error {
			mu.Lock()
			runs++
			mu.Unlock()
			return errors.New("this run failed")
		},
	}

	runner.tick(context.Background())
	runner.tick(context.Background()) // a failed run must still release the guard

	mu.Lock()
	defer mu.Unlock()
	if runs != 2 {
		t.Errorf("runs = %d, want 2; the guard did not release after a failure", runs)
	}
}
