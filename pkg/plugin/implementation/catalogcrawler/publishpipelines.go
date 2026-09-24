package catalogcrawler

// publishpipelines.go is the crawler's side of the scheduled publish
// pipelines: once per tick it asks discovery which capabilities publish, and
// hands each one's record to the pipeline the registry names.
//
// WHAT publishes is discovered in catalogcrawler.go (buildPublishSource /
// publishDiscoverer), beside buildSource and registryDiscoverer. WHERE it
// goes is the sink: every pipeline publishes through the same
// DiscoverySink.Client the crawl path uses, to the provider adapter's
// /publish named by discoveryPushUrl. This file owns only WHEN.
//
// NOTHING HERE LISTS CAPABILITIES. The registry's publish action names a
// pipeline YAML by its repo-relative path; the binary embeds every
// */cataloguepublish-*/ folder under pkg/plugin/implementation (see that
// package's publishpipelines.go); a capability publishes by having both. No
// config list, no import per capability, no Go per capability.
//
// The crawler deliberately owns as little of this as possible. It owns WHEN to
// look (a ticker on its own lifecycle), the registry handle, and the database
// the run log lives in. Whether the registry actually sanctions publishing,
// what a pipeline's cron expression says, and every upstream call belong to
// catalogpublisher/pipeline and to the pipeline file itself.
//
// The tick is a CHECK, not the schedule itself. It fires every few minutes and
// almost always concludes "not due"; each pipeline's own cron expression is
// what decides. That split is why a restart mid-morning does not publish a
// second time: the answer comes from the run log, not from process uptime.

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogcrawler/internal/sink"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogpublisher/pipeline"
)

// Config keys for the scheduled publish pipelines, in the same camelCase style
// as the rest of this plugin's config.
const (
	// cfgPublishPipelines must be "true" for the crawler to sweep the registry
	// for publish pipelines at all. Off by default: a crawler deployment that
	// never asked to publish must not start doing so because a registry it
	// reads gained a publish action.
	cfgPublishPipelines = "publishPipelines"

	// cfgPublishBindingKeys is RETIRED. It used to name the capabilities to
	// publish; the registry is now the only list. A config still setting it is
	// refused rather than ignored, so a deployment relying on it to LIMIT what
	// publishes finds out at startup instead of by publishing more.
	cfgPublishBindingKeys = "publishBindingKeys"

	// cfgPublishEnabled must be "true" for a run to reach the network.
	// Default false: runs still build, which is observable and reversible, but
	// publishing is neither.
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
// Five minutes, not one: the check consults the registry, and the cost of a
// late start is bounded by this interval -- a pipeline due at midnight starts
// by 00:05 at the latest, which for a daily catalogue is indistinguishable
// from on time.
const defaultPublishTickInterval = 5 * time.Minute

// pipelinePublishTimeout bounds each pipeline catalogue's post. Generous, and
// longer than a crawled catalogue's: a state catalogue runs to hundreds of KB
// and the adapter signs, forwards and indexes it before answering.
const pipelinePublishTimeout = 180 * time.Second

// publishConfig is the parsed form of the keys above.
type publishConfig struct {
	enabled bool
	publish bool
	tick    time.Duration
	outDir  string

	// publishURL is the provider adapter's base address, derived from
	// discoveryPushUrl (its /publish endpoint) and handed to every pipeline,
	// so crawled catalogues and pipelines all reach the same /publish.
	publishURL string
}

// publishConfigFrom reads the publish keys.
func publishConfigFrom(config map[string]string) (publishConfig, error) {
	if strings.TrimSpace(config[cfgPublishBindingKeys]) != "" {
		return publishConfig{}, fmt.Errorf(
			"catalogcrawler: config %q is retired; the registry now decides which capabilities publish. "+
				"Remove it and set %q: \"true\" to sweep the registry",
			cfgPublishBindingKeys, cfgPublishPipelines)
	}
	if config[cfgPublishPipelines] != "true" {
		return publishConfig{}, nil
	}
	return publishConfig{
		enabled: true,
		publish: config[cfgPublishEnabled] == "true",
		tick:    durationSecondsOr(config[cfgPublishTickIntervalSec], defaultPublishTickInterval),
		outDir:  strings.TrimSpace(config[cfgPublishCatalogOutputDir]),

		publishURL: publishBase(config[cfgDiscoveryURL]),
	}, nil
}

// publishActionName is the action a record must carry for anything to be
// published. It is the registry's word, and it is duplicated from the
// pipeline package deliberately: this file reads it to skip records quietly,
// while the gate that acts on it lives with the code that enforces it.
const publishActionName = "publish"

// publishBase is discoveryPushUrl without its /publish: the base a
// pipeline.Publisher appends /publish to.
func publishBase(discoveryURL string) string {
	return strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(discoveryURL), "/"), "/publish")
}

// publishSource is what a sweep needs from discovery -- publishDiscoverer in
// catalogcrawler.go, beside buildSource -- as an interface so tick logic is
// testable without a registry.
type publishSource interface {
	Discover(ctx context.Context) ([]publishTarget, error)
}

// publishSweep holds the tick state for every publish pipeline.
type publishSweep struct {
	cfg    publishConfig
	source publishSource
	runLog pipeline.RunLog
	log    *slog.Logger

	// publisher is the sink every pipeline publishes through -- the same
	// Client.Push the crawl path publishes crawled catalogues with.
	publisher pipeline.Publisher

	// run is the pipeline call, injectable so this file's tick logic can be
	// tested without a database or an upstream.
	run func(ctx context.Context, record *model.ProviderRecord, files pipeline.Files) error

	// inFlight guards against a second tick starting while the first is still
	// running. A full run can take minutes; the tick is shorter than that, so
	// without this guard a slow run would be joined by another one doubling
	// every upstream call and racing to write the same files.
	inFlight sync.Mutex
	running  bool
}

// tick asks discovery what the registry sanctions and runs each pipeline.
//
// Every failure here is logged and swallowed, and one capability's failure
// never stops the next. This loop shares a Scheduler with the index-poll and
// catalog-sync loops, which are unrelated work: a registry outage must not
// take them down with it.
func (p *publishSweep) tick(ctx context.Context) {
	if !p.claim() {
		p.log.InfoContext(ctx, "catalogcrawler: publish sweep still running, skipping this tick")
		return
	}
	defer p.release()

	targets, err := p.source.Discover(ctx)
	if err != nil {
		p.log.ErrorContext(ctx, "catalogcrawler: publish discovery failed", "error", err)
		return
	}
	for _, target := range targets {
		if err := p.run(ctx, target.record, target.files); err != nil {
			p.log.ErrorContext(ctx, "catalogcrawler: publish pipeline run failed",
				"bindingKey", target.record.BindingKey, "error", err)
		}
	}
}

// claim reports whether this tick may run; release must follow a true claim.
func (p *publishSweep) claim() bool {
	p.inFlight.Lock()
	defer p.inFlight.Unlock()
	if p.running {
		return false
	}
	p.running = true
	return true
}

func (p *publishSweep) release() {
	p.inFlight.Lock()
	p.running = false
	p.inFlight.Unlock()
}

// runPipeline is the real run, calling the frame.
func (p *publishSweep) runPipeline(ctx context.Context, record *model.ProviderRecord, files pipeline.Files) error {
	report, err := pipeline.Run(ctx, pipeline.RunOptions{
		Pipeline: files,
		Record:   record,
		RunLog:   p.runLog,
		OutDir:   p.cfg.outDir,
		Publish:  p.cfg.publish,
		Log:      p.log,

		PublishURL: p.cfg.publishURL,
		Publisher:  p.publisher,
	})
	if err != nil {
		return err
	}
	if !report.Due {
		p.log.DebugContext(ctx, "catalogcrawler: publish pipeline not due",
			"capability", report.Capability, "reason", report.Reason)
		return nil
	}

	published := 0
	if report.Published != nil {
		published = len(report.Published.Outcomes)
	}
	p.log.InfoContext(ctx, "catalogcrawler: publish pipeline ran",
		"capability", report.Capability, "pipeline", report.PipelinePath,
		"catalogs", len(report.Catalogues), "collectionErrors", report.Errors,
		"counters", report.Counters, "published", published, "publishEnabled", p.cfg.publish)
	return nil
}

// newPublishSweep wires the sweep over a discovery source.
func newPublishSweep(cfg publishConfig, source publishSource, runLog pipeline.RunLog, log *slog.Logger) *publishSweep {
	sweep := &publishSweep{
		cfg:    cfg,
		source: source,
		runLog: runLog,
		log:    log,

		// Publish posts to the base it is given (cfg.publishURL, via
		// RunOptions.PublishURL), so the sink's own endpoint and identity are
		// unused on this path: pipeline bodies are complete envelopes.
		publisher: sink.NewDiscoverySink("", "", "", 0, pipelinePublishTimeout),
	}
	sweep.run = sweep.runPipeline
	return sweep
}

// servedActions lists a record's actions, sorted, for a log line.
func servedActions(record *model.ProviderRecord) string {
	served := make([]string, 0, len(record.Actions))
	for action := range record.Actions {
		served = append(served, action)
	}
	sort.Strings(served)
	return strings.Join(served, ",")
}
