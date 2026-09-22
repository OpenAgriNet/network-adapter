package catalogcrawler

// publishpipelines.go is the crawler's side of the scheduled publish
// pipelines: it reads the configuration, asks the registry once per tick which
// capabilities this deployment is bound to, and hands each record to the
// pipeline that serves it.
//
// The crawler deliberately owns as little of this as possible. It owns WHEN to
// look (a ticker on its own lifecycle), the registry handle, and the database
// the run log lives in. Whether the registry actually sanctions publishing,
// what a pipeline's cron expression says, and every upstream call belong to
// internal/pipeline and to the capability packages that implement its
// Collector.
//
// The tick is a CHECK, not the schedule itself. It fires every few minutes and
// almost always concludes "not due"; each pipeline's own cron expression is
// what decides. That split is why a restart mid-morning does not publish a
// second time: the answer comes from the run log, not from process uptime.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	agmarket "github.com/beckn-one/beckn-onix/pkg/plugin/implementation/MandiPrice/cataloguepublish-agmarket"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/pipeline"
)

// collectors is the one place that changes when a deployment gains a pipeline:
// the capability code, and the Collector compiled in to serve it.
//
// An explicit table rather than registration through init(): it makes visible
// at compile time what loadRegistryPipeline already asserts at runtime -- this
// binary can only run the pipelines it embeds -- and a reader can answer "what
// can this build publish" by reading four lines.
//
// The cost is a compile-time dependency on every capability's publish package,
// and an .so that carries each one's embedded mappings. At five pipelines that
// is worth revisiting; at one or two it is cheaper than the indirection.
var collectors = map[string]pipeline.Collector{
	agmarket.Capability: agmarket.Collector{},
}

// Config keys for the scheduled publish pipelines, in the same camelCase style
// as the rest of this plugin's config.
const (
	// cfgPublishBindingKeys is a comma-separated list of
	// "<participantId>|<capabilityCode>". Supplying it is what turns
	// publishing on: a deployment that names no capability has nothing to
	// publish.
	cfgPublishBindingKeys = "publishBindingKeys"

	// cfgPublishEnabled must be "true" for a run to reach the network.
	// Default false: runs still collect and build, which is observable and
	// reversible, but publishing is neither.
	cfgPublishEnabled = "publishEnabled"

	// cfgPublishTickIntervalSec is how often to CHECK whether a pipeline is
	// due -- not how often one runs.
	cfgPublishTickIntervalSec = "publishTickIntervalSeconds"

	// cfgPublishCatalogOutputDir keeps built catalogues for inspection. Each
	// pipeline gets its own subdirectory beneath it. Empty means a temporary
	// directory each run removes.
	cfgPublishCatalogOutputDir = "publishCatalogOutputDir"
)

// defaultPublishTickInterval is how often the due-ness check runs.
//
// Five minutes, not one: the check is cheap but it does consult the registry,
// and the cost of a late start is bounded by this interval -- a pipeline due
// at midnight starts by 00:05 at the latest, which for a daily catalogue is
// indistinguishable from on time.
const defaultPublishTickInterval = 5 * time.Minute

// publishConfig is the parsed form of the keys above.
type publishConfig struct {
	enabled     bool
	bindingKeys []string
	publish     bool
	tick        time.Duration
	outDir      string
}

// publishConfigFrom reads the publish keys, refusing a binding key that could
// never resolve rather than letting every tick fail against the registry.
func publishConfigFrom(config map[string]string) (publishConfig, error) {
	keys := splitNonEmpty(config[cfgPublishBindingKeys])
	if len(keys) == 0 {
		return publishConfig{}, nil
	}

	for _, key := range keys {
		parts := strings.Split(key, "|")
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			return publishConfig{}, fmt.Errorf(
				"catalogcrawler: config %q entry %q must be \"<participantId>|<capabilityCode>\"",
				cfgPublishBindingKeys, key)
		}
		capability := strings.TrimSpace(parts[1])
		if _, ok := collectors[capability]; !ok {
			return publishConfig{}, fmt.Errorf(
				"catalogcrawler: config %q names capability %q, which this binary carries no pipeline for; it has %s",
				cfgPublishBindingKeys, capability, knownCapabilities())
		}
	}

	return publishConfig{
		enabled:     true,
		bindingKeys: keys,
		publish:     config[cfgPublishEnabled] == "true",
		tick:        durationSecondsOr(config[cfgPublishTickIntervalSec], defaultPublishTickInterval),
		outDir:      strings.TrimSpace(config[cfgPublishCatalogOutputDir]),
	}, nil
}

// knownCapabilities lists what this binary can publish, for an error that says
// what is available rather than only what is missing.
func knownCapabilities() string {
	if len(collectors) == 0 {
		return "no publish pipelines at all"
	}
	names := make([]string, 0, len(collectors))
	for capability := range collectors {
		names = append(names, strconv.Quote(capability))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// publishRunner holds one pipeline's tick state.
type publishRunner struct {
	bindingKey string
	capability string
	collector  pipeline.Collector
	cfg        publishConfig

	lookup definition.ProviderRecordLookup
	runLog pipeline.RunLog
	log    *slog.Logger

	// run is the pipeline call, injectable so this file's tick logic can be
	// tested without a registry, a database, or an upstream.
	run func(ctx context.Context, record *model.ProviderRecord) error

	// inFlight guards against a second tick starting while the first is still
	// collecting. A full run can take minutes; the tick is shorter than that,
	// so without this guard a slow run would be joined by another one
	// doubling every upstream call and racing to write the same files.
	inFlight sync.Mutex
	running  bool
}

// tick checks whether this pipeline should run, and runs it if so.
//
// Every failure here is logged and swallowed. This loop shares a Scheduler
// with the index-poll and catalog-sync loops, which are unrelated work: a
// registry outage must not take them down with it.
func (p *publishRunner) tick(ctx context.Context) {
	if !p.claim() {
		p.log.InfoContext(ctx, "catalogcrawler: publish pipeline still running, skipping this tick",
			"bindingKey", p.bindingKey)
		return
	}
	defer p.release()

	record, err := p.lookup.ProviderRecord(ctx, p.bindingKey)
	if errors.Is(err, definition.ErrProviderRecordNotFound) {
		// The registry answering "no" is a normal state, not a fault: the
		// capability may simply not be registered in this environment.
		p.log.InfoContext(ctx, "catalogcrawler: capability is not registered; nothing to publish",
			"bindingKey", p.bindingKey)
		return
	}
	if err != nil {
		p.log.ErrorContext(ctx, "catalogcrawler: could not consult the registry for a publish pipeline",
			"bindingKey", p.bindingKey, "error", err)
		return
	}

	if err := p.run(ctx, record); err != nil {
		p.log.ErrorContext(ctx, "catalogcrawler: publish pipeline run failed",
			"bindingKey", p.bindingKey, "error", err)
	}
}

// claim reports whether this tick may run; release must follow a true claim.
func (p *publishRunner) claim() bool {
	p.inFlight.Lock()
	defer p.inFlight.Unlock()
	if p.running {
		return false
	}
	p.running = true
	return true
}

func (p *publishRunner) release() {
	p.inFlight.Lock()
	p.running = false
	p.inFlight.Unlock()
}

// runPipeline is the real run, calling the frame.
func (p *publishRunner) runPipeline(ctx context.Context, record *model.ProviderRecord) error {
	report, err := pipeline.Run(ctx, pipeline.RunOptions{
		Collector: p.collector,
		Record:    record,
		RunLog:    p.runLog,
		OutDir:    p.cfg.outDir,
		Publish:   p.cfg.publish,
		Log:       p.log,
	})
	if err != nil {
		return err
	}
	if !report.Due {
		p.log.DebugContext(ctx, "catalogcrawler: publish pipeline not due",
			"capability", p.capability, "reason", report.Reason)
		return nil
	}

	published := 0
	if report.Published != nil {
		published = len(report.Published.Outcomes)
	}
	p.log.InfoContext(ctx, "catalogcrawler: publish pipeline ran",
		"capability", p.capability, "pipeline", report.PipelinePath,
		"catalogs", len(report.Catalogues), "collectionErrors", report.Errors,
		"counters", report.Counters, "published", published, "publishEnabled", p.cfg.publish)
	return nil
}

// newPublishRunners wires one runner per configured binding key, or nil when
// publishing is not configured.
//
// The registry plugin must implement ProviderRecordLookup: without it there is
// no way to learn whether a capability is sanctioned to publish, and the only
// alternative -- publishing anyway -- is exactly what the gate exists to
// prevent. So a configured pipeline with an incapable registry is a startup
// error, not a silently disabled feature.
func newPublishRunners(cfg publishConfig, registry definition.RegistryLookup,
	runLog pipeline.RunLog, log *slog.Logger) ([]*publishRunner, error) {
	if !cfg.enabled {
		return nil, nil
	}
	lookup, ok := registry.(definition.ProviderRecordLookup)
	if !ok {
		return nil, fmt.Errorf(
			"catalogcrawler: config %q needs a registry plugin that supports ProviderRecordLookup (e.g. sunbirdRegistry)",
			cfgPublishBindingKeys)
	}

	runners := make([]*publishRunner, 0, len(cfg.bindingKeys))
	for _, key := range cfg.bindingKeys {
		capability := strings.TrimSpace(strings.SplitN(key, "|", 2)[1])
		runner := &publishRunner{
			bindingKey: key,
			capability: capability,
			collector:  collectors[capability], // presence checked in publishConfigFrom
			cfg:        cfg,
			lookup:     lookup,
			runLog:     runLog,
			log:        log,
		}
		runner.run = runner.runPipeline
		runners = append(runners, runner)
	}
	return runners, nil
}
