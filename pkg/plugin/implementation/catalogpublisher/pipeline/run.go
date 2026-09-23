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
	"path/filepath"
	"strings"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/catalogpublish"
)

// Inputs every pipeline must declare, because the frame itself uses them: the
// upstream's address and the credentials it exchanges for a token. A pipeline
// naming them differently would resolve them into a map the frame cannot read,
// so the convention is enforced rather than assumed.
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
	// Collector is the domain half. Required.
	Collector Collector

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
	if opts.Collector == nil {
		return RunReport{}, fmt.Errorf("no collector: a pipeline with no domain half has nothing to collect")
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
	capability := opts.Collector.Capability()

	// 1. The registry gate, and the pipeline it names.
	pipelinePath, err := PipelinePathFor(opts.Record)
	if err != nil {
		return RunReport{Capability: capability}, err
	}
	spec, err := loadRegistryPipeline(opts.Collector, pipelinePath)
	if err != nil {
		return RunReport{Capability: capability}, err
	}
	report := RunReport{
		Capability:   capability,
		PipelinePath: pipelinePath,
		Unpersisted:  opts.RunLog == nil,
	}

	// The registry may sanction a pipeline whose file serves a different
	// capability than the collector claims. That mismatch would key the run
	// log and the output directory on one name while publishing under
	// another.
	if declared := strings.TrimSpace(spec.Metadata.Capability); declared != capability {
		return report, fmt.Errorf("collector serves %q but %s declares capability %q",
			capability, pipelinePath, declared)
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
	for _, required := range []string{inputBaseURL, inputTokenUser, inputTokenSecret} {
		if resolved[required] == "" {
			return fmt.Errorf("input %q is empty; the pipeline cannot reach the upstream without it", required)
		}
	}

	files := opts.Collector.Pipeline()

	// The mappings are served over loopback because jsonmapper resolves them
	// by URL; they are embedded in the binary, not read from disk.
	mappingBase, stopMappings, err := catalogpublish.ServeMappings(files.FS, "mappings")
	if err != nil {
		return fmt.Errorf("serving the pipeline's mappings: %w", err)
	}
	defer stopMappings()

	mapper, closeMapper, err := catalogpublish.NewMapper(ctx)
	if err != nil {
		return fmt.Errorf("building the mapper: %w", err)
	}
	defer func() { _ = closeMapper() }()

	client := NewClient(resolved[inputBaseURL])
	token, err := client.Token(ctx, resolved[inputTokenUser], resolved[inputTokenSecret])
	if err != nil {
		return fmt.Errorf("exchanging credentials: %w", err)
	}

	outDir, cleanup, err := pipelineDir(opts)
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

	collected, err := opts.Collector.Collect(ctx, RunEnv{
		Spec:        spec,
		Inputs:      resolved,
		Mapper:      mapper,
		MappingBase: mappingBase,
		Client:      client,
		Token:       token,
		OutDir:      outDir,
		Log:         log,
	})
	if err != nil {
		return fmt.Errorf("collecting: %w", err)
	}
	report.Catalogues = collected.Catalogues
	report.Errors = collected.Errors
	report.Counters = collected.Counters

	if err := WriteCatalogues(collected.Catalogues, outDir, prefix); err != nil {
		return fmt.Errorf("writing catalogues: %w", err)
	}
	log.InfoContext(ctx, "publish pipeline: built catalogues",
		"capability", report.Capability, "catalogs", len(collected.Catalogues),
		"collectionErrors", collected.Errors, "counters", collected.Counters, "dir", outDir)

	if !opts.Publish {
		return nil
	}
	result, err := PublishCatalogues(ctx, spec.Publish, resolved, outDir, prefix, collected.Errors)
	report.Published = &result
	if err != nil {
		return fmt.Errorf("publishing catalogues: %w", err)
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
func pipelineDir(opts RunOptions) (string, func(), error) {
	slug := strings.NewReplacer("/", "-", ":", "-", " ", "-").Replace(opts.Collector.Capability())

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
