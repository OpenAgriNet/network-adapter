package catalogcrawler

// publishpipelines.go is the crawler's side of the scheduled publish
// pipelines: once per tick it asks the registry which capabilities publish,
// and hands each one's record to the pipeline the registry names.
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
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation"
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

// publishConfig is the parsed form of the keys above.
type publishConfig struct {
	enabled bool
	publish bool
	tick    time.Duration
	outDir  string
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
	}, nil
}

// publishActionName is the action a record must carry for anything to be
// published. It is the registry's word, and it is duplicated from the
// pipeline package deliberately: this file reads it to skip records quietly,
// while the gate that acts on it lives with the code that enforces it.
const publishActionName = "publish"

// registry is what a sweep needs from the registry plugin: the list of
// bindings, and each one's record.
type registry interface {
	definition.ProviderBindingLister
	definition.ProviderRecordLookup
}

// publishSweep holds the tick state for every publish pipeline.
type publishSweep struct {
	cfg      publishConfig
	registry registry
	runLog   pipeline.RunLog
	log      *slog.Logger

	// resolve turns the registry's pipeline path into the embedded files.
	// Injectable so tick logic is testable without the real embed.
	resolve func(registryPath string) (pipeline.Files, error)

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

// tick sweeps the registry and runs every pipeline it sanctions.
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

	keys, err := p.registry.ProviderBindingKeys(ctx)
	if err != nil {
		p.log.ErrorContext(ctx, "catalogcrawler: could not list the registry's capabilities", "error", err)
		return
	}
	p.log.InfoContext(ctx, "catalogcrawler: publish sweep", "capabilities", len(keys))

	for _, key := range keys {
		p.runOne(ctx, key)
	}
}

// runOne resolves one binding and runs its pipeline if it publishes.
func (p *publishSweep) runOne(ctx context.Context, key string) {
	record, err := p.registry.ProviderRecord(ctx, key)
	if errors.Is(err, definition.ErrProviderRecordNotFound) {
		// Listed but not usable: inactive, unowned, or no active actions.
		// The registry plugin has already logged which.
		p.log.DebugContext(ctx, "catalogcrawler: binding is not usable; skipping", "bindingKey", key)
		return
	}
	if err != nil {
		p.log.ErrorContext(ctx, "catalogcrawler: could not consult the registry for a publish pipeline",
			"bindingKey", key, "error", err)
		return
	}

	// Most capabilities are consumed, not published. Quiet, not an error.
	if _, publishes := record.Actions[publishActionName]; !publishes {
		p.log.DebugContext(ctx, "catalogcrawler: binding serves no publish action", "bindingKey", key)
		return
	}

	pipelinePath, err := pipeline.PipelinePathFor(record)
	if err != nil {
		p.log.ErrorContext(ctx, "catalogcrawler: publish action names no pipeline", "bindingKey", key, "error", err)
		return
	}
	files, err := p.resolve(pipelinePath)
	if err != nil {
		// The registry sanctions a pipeline this binary was not built with.
		// A deployment problem, said loudly with what IS available.
		p.log.ErrorContext(ctx, "catalogcrawler: the registry names a pipeline this binary does not carry",
			"bindingKey", key, "pipeline", pipelinePath, "carried", implementation.PublishPipelines(), "error", err)
		return
	}

	p.log.InfoContext(ctx, "catalogcrawler: registry sanctions publishing",
		"bindingKey", key, "actions", servedActions(record), "pipeline", pipelinePath)
	if err := p.run(ctx, record, files); err != nil {
		p.log.ErrorContext(ctx, "catalogcrawler: publish pipeline run failed",
			"bindingKey", key, "error", err)
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

// newPublishSweep wires the sweep, or returns nil when publishing is not
// configured.
//
// The registry plugin must both list bindings and resolve them: without the
// list there is nothing to sweep, and without the lookup there is no way to
// learn whether a capability is sanctioned to publish. So a configured sweep
// with an incapable registry is a startup error, not a silently disabled
// feature.
func newPublishSweep(cfg publishConfig, lookup definition.RegistryLookup,
	runLog pipeline.RunLog, log *slog.Logger) (*publishSweep, error) {
	if !cfg.enabled {
		return nil, nil
	}
	reg, ok := lookup.(registry)
	if !ok {
		return nil, fmt.Errorf(
			"catalogcrawler: config %q needs a registry plugin that can list and resolve provider bindings (e.g. sunbirdRegistry)",
			cfgPublishPipelines)
	}

	sweep := &publishSweep{
		cfg:      cfg,
		registry: reg,
		runLog:   runLog,
		log:      log,
		resolve:  implementation.PublishPipeline,
	}
	sweep.run = sweep.runPipeline
	return sweep, nil
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
