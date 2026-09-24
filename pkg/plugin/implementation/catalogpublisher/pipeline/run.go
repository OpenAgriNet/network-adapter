// Package pipeline is the shared frame for scheduled publish pipelines: the
// registry gate, the cron schedule, the run log, input resolution, the
// upstream client, and the publish step. Everything in it is the same for
// every pipeline; what differs is a Collector, which a capability package
// implements (see collector.go).
//
// It sits beside catalogpublish under catalogpublisher/ so BOTH the crawler
// that ticks a pipeline and the capability packages that define one can import
// it. It must NOT be moved under an internal/ directory: that would put one of
// those two consumers out of reach, which has already happened twice.
//
// It is NOT part of the catalogpublisher plugin's own work, and that plugin
// does not import it. catalogpublisher serves the decentralized-catalog path
// (RFC NFH-014), writing signed blobs to a store that crawlers walk. This
// frame builds catalogues and posts them to the provider adapter, which
// reaches the discovery service. Shared parent directory, opposite directions.
package pipeline

// run.go is the frame: the one exported entry point that turns "the registry
// says this capability publishes, and the clock says it is due" into a day's
// catalogues on the network.
//
// Everything here is the same for every pipeline. What differs -- what to
// fetch, how to shape it, what a catalogue of it contains -- is the
// Collector's, and this file never inspects it.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogpublisher/catalogpublish"
)

// Inputs every pipeline WITH AN UPSTREAM must declare, because the frame
// itself uses them: the upstream's address and the credentials it exchanges
// for a token. A pipeline naming them differently would resolve them into a
// map the frame cannot read, so the convention is enforced rather than
// assumed. A pipeline with no `upstream:` block needs none of them.
const (
	inputBaseURL     = "baseUrl"
	inputTokenUser   = "tokenUser"
	inputTokenSecret = "tokenSecret"
)

// RunLog is where a run's timestamp outlives the process.
//
// Without it, a restart at 00:05 re-runs a pipeline that already ran at 00:01
// -- the schedule alone cannot tell the difference, because "has it run" is
// not a fact about the clock. The method names match the crawler plugin's
// store exactly, so its *store.Store satisfies this without an adapter.
type RunLog interface {
	LastPipelineRun(ctx context.Context, pipeline string) (time.Time, error)
	RecordPipelineRun(ctx context.Context, pipeline string, at time.Time) error
}

// RunOptions is everything a run does not decide for itself.
type RunOptions struct {
	// Pipeline is the capability's YAML and mappings. Required: it is the
	// program this run executes.
	Pipeline Files

	// Record is the registry's answer for this capability. Required: it is
	// the only thing that sanctions publishing at all.
	Record *model.ProviderRecord

	// RunLog persists "when did this last run". Optional, and omitting it is
	// a real choice with a real cost -- see RunReport.Unpersisted.
	RunLog RunLog

	// Lookup resolves the pipeline's declared inputs. nil means os.LookupEnv.
	Lookup func(string) (string, bool)

	// Now is the instant the schedule is judged against. Zero means
	// time.Now(). A test fixes it; production leaves it zero.
	Now time.Time

	// OutDir is where catalogues are written before publishing. Empty means a
	// temporary directory removed when the run finishes.
	//
	// Each pipeline gets its OWN subdirectory beneath it, named for the
	// capability. Sharing one directory would be silent corruption: the stale
	// sweep below deletes by filename prefix and the publish step globs by
	// the same prefix, so two pipelines in one directory would delete and
	// then publish each other's catalogues.
	OutDir string

	// Publish sends the built catalogues to the network. It defaults to
	// FALSE: building is observable and reversible, publishing is neither,
	// so it is opted into rather than out of.
	Publish bool

	// DryRun stops after the decision, before any fetching.
	DryRun bool

	Log *slog.Logger
}

// RunReport is what happened, in the terms an operator asks about.
type RunReport struct {
	Capability   string
	PipelinePath string
	Due          bool
	Reason       string

	// Unpersisted is true when no RunLog was supplied, so this run's outcome
	// is not remembered and a restart will run it again. Reported rather than
	// assumed, because the failure it causes -- republishing -- looks like
	// normal operation in every log.
	Unpersisted bool

	// Catalogues, Errors and Counters are the collector's own result. Errors
	// counts parts of the collection that failed for a real reason; Counters
	// are the domain's numbers, printed but not interpreted.
	Catalogues []Catalogue
	Errors     int
	Counters   map[string]int

	OutDir string

	// Published is nil when the run built catalogues without sending them.
	Published *catalogpublish.Result
}

// Run executes one tick of a publish pipeline.
//
// The order is deliberate and each step is a refusal point: the registry gate
// first (is this capability allowed to publish at all), then the run log (has
// this firing already been served), then the schedule (is it time), and only
// then any network traffic. Everything that can say "no" says it before
// anything is fetched.
//
// A failed run is deliberately NOT recorded, so a transient upstream outage at
// midnight is retried on the next tick rather than costing the whole day.
func Run(ctx context.Context, opts RunOptions) (RunReport, error) {
	if opts.Pipeline.Path == "" {
		return RunReport{}, fmt.Errorf("no pipeline: RunOptions.Pipeline names no YAML to run")
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	lookup := opts.Lookup
	if lookup == nil {
		lookup = os.LookupEnv
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	// 1. The registry gate, and the pipeline it names.
	pipelinePath, err := PipelinePathFor(opts.Record)
	if err != nil {
		return RunReport{}, err
	}
	spec, err := loadRegistryPipeline(opts.Pipeline, pipelinePath)
	if err != nil {
		return RunReport{}, err
	}

	// The capability comes from the file itself. Nothing in Go declares it,
	// so nothing in Go can disagree with it.
	capability := strings.TrimSpace(spec.Metadata.Capability)
	if capability == "" {
		return RunReport{}, fmt.Errorf("%s declares no metadata.capability", pipelinePath)
	}
	report := RunReport{
		Capability:   capability,
		PipelinePath: pipelinePath,
		Unpersisted:  opts.RunLog == nil,
	}

	// 2. When did it last run. An unreadable log is fatal: "has this firing
	// been served" is then unknown, and the guess that publishes anyway is
	// the one with consequences.
	var lastRun time.Time
	if opts.RunLog != nil {
		if lastRun, err = opts.RunLog.LastPipelineRun(ctx, capability); err != nil {
			return report, fmt.Errorf("reading the run log for %s: %w", capability, err)
		}
	}

	// 3. Is it due.
	due, reason, err := dueNow(spec.Schedule, now, lastRun)
	if err != nil {
		return report, err
	}
	report.Due, report.Reason = due, reason
	if !due {
		log.InfoContext(ctx, "publish pipeline: not due", "capability", capability, "reason", reason)
		return report, nil
	}
	if opts.DryRun {
		log.InfoContext(ctx, "publish pipeline: due (dry run, nothing fetched)",
			"capability", capability, "reason", reason)
		return report, nil
	}

	// 4. Do the work.
	if err := execute(ctx, spec, lookup, opts, &report, log); err != nil {
		return report, err
	}

	// 5. Only a run that finished is a run that happened.
	if opts.RunLog != nil {
		if err := opts.RunLog.RecordPipelineRun(ctx, capability, now); err != nil {
			// The work is done and published; failing here would make a
			// caller retry a completed run. Loud, but not fatal.
			log.ErrorContext(ctx, "publish pipeline: run finished but could not be recorded; it may run again",
				"capability", capability, "error", err)
		}
	}
	return report, nil
}

// execute prepares everything the collector needs, hands off, then writes and
// publishes what comes back.
func execute(ctx context.Context, spec Spec, lookup func(string) (string, bool),
	opts RunOptions, report *RunReport, log *slog.Logger) error {
	resolved, err := ResolveInputs(spec, lookup)
	if err != nil {
		return fmt.Errorf("resolving pipeline inputs: %w", err)
	}
	if hasUpstream(spec) {
		for _, required := range []string{inputBaseURL, inputTokenUser, inputTokenSecret} {
			if resolved[required] == "" {
				return fmt.Errorf("input %q is empty; the pipeline cannot reach the upstream without it", required)
			}
		}
	}

	// The mappings are served over loopback because jsonmapper resolves them
	// by URL; they are embedded in the binary, not read from disk.
	//
	// They are found NEXT TO the pipeline file rather than at the root of the
	// embedded filesystem, because that is what a file's `mapping:` references
	// are relative to -- and because a capability is free to put its pipeline
	// wherever it likes inside its own package.
	mappingsDir := path.Join(path.Dir(opts.Pipeline.Path), "mappings")
	mappingBase, stopMappings, err := catalogpublish.ServeMappings(opts.Pipeline.FS, mappingsDir)
	if err != nil {
		return fmt.Errorf("serving the pipeline's mappings: %w", err)
	}
	defer stopMappings()

	mapper, closeMapper, err := catalogpublish.NewMapper(ctx)
	if err != nil {
		return fmt.Errorf("building the mapper: %w", err)
	}
	defer func() { _ = closeMapper() }()

	// A pipeline with no upstream has nothing to authenticate to and nothing
	// to call: no token, no client. Its steps are const/derive/filter and the
	// like; an HTTP step among them is refused by the schema at load, and by
	// the runner as a backstop.
	rc := newRunContext(resolved, "")
	var client *Client
	if hasUpstream(spec) {
		log.InfoContext(ctx, "publish pipeline: exchanging credentials",
			"upstream", resolved[inputBaseURL], "path", spec.Upstream.Auth.Request.Path)
		client = NewClient(resolved[inputBaseURL])
		token, err := client.Token(ctx, spec.Upstream.Auth, rc)
		if err != nil {
			return fmt.Errorf("exchanging credentials: %w", err)
		}
		rc = newRunContext(resolved, token)
		log.InfoContext(ctx, "publish pipeline: token exchanged", "characters", len(token))
	} else {
		log.InfoContext(ctx, "publish pipeline: no upstream declared; skipping credentials")
	}

	outDir, cleanup, err := pipelineDir(opts, report.Capability)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}
	report.OutDir = outDir

	prefix := filenamePrefix(spec)

	// Yesterday's files in a reused directory would be published again today
	// as though they were fresh.
	if err := RemoveStaleCatalogues(outDir, prefix); err != nil {
		return fmt.Errorf("clearing previous catalogues: %w", err)
	}

	cache, err := newExprCache()
	if err != nil {
		return err
	}

	// The steps the file declares, in the order it declares them.
	runner := &stepRunner{
		spec: spec, rc: rc, cache: cache, client: client,
		mapper: mapper, mappingBase: mappingBase, log: log,
		counters: map[string]int{},
	}
	log.InfoContext(ctx, "publish pipeline: running steps", "steps", len(spec.Pipeline))
	records, err := runner.runSteps(ctx)
	if err != nil {
		return fmt.Errorf("running the pipeline's steps: %w", err)
	}
	log.InfoContext(ctx, "publish pipeline: steps done", "records", len(records))

	// The catalogues the file's `catalog:` block describes.
	log.InfoContext(ctx, "publish pipeline: building catalogues",
		"groupBy", spec.Catalog.GroupBy, "budget", spec.Catalog.Chunk.Budget)
	catalogues, buildCounters, err := buildCatalogues(ctx, spec.Catalog, records, rc, cache, mapper, mappingBase)
	if err != nil {
		return fmt.Errorf("building catalogues: %w", err)
	}

	// Both halves report into one set of counters, named by the file.
	counters := runner.counters
	for name, count := range buildCounters {
		counters[name] += count
	}
	report.Catalogues = catalogues
	report.Counters = counters
	report.Errors = collectionErrors(spec.Publish, counters)

	if err := WriteCatalogues(catalogues, outDir, prefix); err != nil {
		return fmt.Errorf("writing catalogues: %w", err)
	}
	log.InfoContext(ctx, "publish pipeline: built catalogues",
		"capability", report.Capability, "catalogs", len(catalogues),
		"collectionErrors", report.Errors, "counters", counters, "dir", outDir)

	if !opts.Publish {
		return nil
	}
	log.InfoContext(ctx, "publish pipeline: PUBLISHING to the network",
		"catalogues", len(catalogues), "target", resolved["publishUrl"])
	result, err := PublishCatalogues(ctx, spec.Publish, resolved, outDir, prefix, report.Errors)
	report.Published = &result
	if err != nil {
		return fmt.Errorf("publishing catalogues: %w", err)
	}
	for _, outcome := range result.Outcomes {
		log.InfoContext(ctx, "publish pipeline: outcome",
			"catalogId", outcome.CatalogID, "status", outcome.Status, "reason", outcome.Reason)
	}
	if result.HasFailures() {
		return fmt.Errorf("at least one catalogue did not reach the network intact")
	}
	return nil
}

// pipelineDir gives this pipeline a directory of its own.
//
// When the caller named one, this pipeline gets a subdirectory beneath it
// rather than the directory itself -- see RunOptions.OutDir for why sharing
// would be silent corruption. When the caller named none, a temporary
// directory is made and removed, and the returned cleanup is non-nil.
func pipelineDir(opts RunOptions, capability string) (string, func(), error) {
	slug := strings.NewReplacer("/", "-", ":", "-", " ", "-").Replace(capability)

	if opts.OutDir == "" {
		dir, err := os.MkdirTemp("", "catalogs-"+slug+"-")
		if err != nil {
			return "", nil, fmt.Errorf("making a directory for the catalogues: %w", err)
		}
		return dir, func() { _ = os.RemoveAll(dir) }, nil
	}

	dir := filepath.Join(opts.OutDir, slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, fmt.Errorf("making %s: %w", dir, err)
	}
	return dir, nil, nil
}

// filenamePrefix names this pipeline's files on disk. Three things must agree
// on it: what writes the files, what sweeps stale ones, and the glob the
// publish step sends from.
func filenamePrefix(spec Spec) string {
	if name := strings.TrimSpace(spec.Metadata.Name); name != "" {
		return name
	}
	return "catalog"
}

// collectionErrors reads the counter the file's publish.refuseWhen names.
//
// The rule is `collection.<counter> > 0`, and <counter> is whatever the
// pipeline's own steps record into -- mandi calls it stateErrors, another
// pipeline will call it something else. Resolving it by name rather than
// hardcoding one keeps the refusal the file's decision.
//
// A file with no refuseWhen has no refusal, and reports zero.
func collectionErrors(spec Publish, counters map[string]int) int {
	counter, ok := refuseWhenCounter(spec.RefuseWhen)
	if !ok {
		return 0
	}
	return counters[counter]
}

// hasUpstream reports whether the file declares an upstream at all.
//
// The absence of the block is the signal, not an auth kind: a pipeline whose
// catalogue is fixed has no service to name, and asking it to invent a
// baseUrl and credentials it never uses would make every such file lie.
func hasUpstream(spec Spec) bool {
	return strings.TrimSpace(spec.Upstream.BaseURL) != ""
}
