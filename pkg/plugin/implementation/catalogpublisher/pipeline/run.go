// Package pipeline is the shared frame for scheduled publish pipelines: the
// registry gate, the cron schedule, the run log, input resolution, the
// upstream client, and the publish step. Everything in it is the same for
// every pipeline; what differs -- what to fetch, how to shape it, what a
// catalog of it contains -- is declared in the pipeline's own YAML (steps.go,
// catalog.go interpret it), and this package never hardcodes a capability.
//
// It must NOT be moved under an internal/ directory: the crawler that ticks a
// pipeline imports it, and so do the capability folders' tests.
//
// A pipeline is a hosted file: the registry's publish action names its https
// URL, and a run fetches it (remote.go), exactly as the adapter fetches every
// other mapping.
//
// It builds catalogs and decides what may be published; it does not make
// the HTTP call. A run that publishes is given a Publisher -- the crawler's
// sink -- so exactly one piece of code posts to the provider adapter.
//
// It is NOT part of the catalogpublisher plugin's own work, and that plugin
// does not import it. catalogpublisher serves the decentralized-catalog path
// (RFC NFH-014), writing signed blobs to a store that crawlers walk.
package pipeline

// run.go is the frame: the one exported entry point that turns "the registry
// says this capability publishes, and the clock says it is due" into a day's
// catalogs on the network.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
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
//
// The key is the pipeline's URL (Files.URL), never the capability.
type RunLog interface {
	LastPipelineRun(ctx context.Context, pipeline string) (time.Time, error)
	RecordPipelineRun(ctx context.Context, pipeline string, at time.Time) error
}

// RunClaimer is a RunLog that can claim a firing before the run does it, so
// two replicas ticking at once do not both run and both publish. Optional: a
// RunLog without it runs unclaimed, which is correct for one replica.
//
// ClaimPipelineRun sets the marker to now only while it is still before
// firing -- the start of the current schedule window -- and reports whether
// it did. ReleasePipelineRun restores the marker to previous after a failed
// run, so the firing is retried.
type RunClaimer interface {
	ClaimPipelineRun(ctx context.Context, pipeline string, now, firing time.Time) (bool, error)
	ReleasePipelineRun(ctx context.Context, pipeline string, previous time.Time) error
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

	// OutDir is where catalogs are written before publishing. Empty means a
	// temporary directory removed when the run finishes.
	//
	// Each pipeline gets its OWN subdirectory beneath it, named for the
	// capability. Sharing one directory would be silent corruption: the stale
	// sweep below deletes by filename prefix and the publish step globs by
	// the same prefix, so two pipelines in one directory would delete and
	// then publish each other's catalogs.
	OutDir string

	// Publish sends the built catalogs to the network. It defaults to
	// FALSE: building is observable and reversible, publishing is neither,
	// so it is opted into rather than out of.
	Publish bool

	// PublishURL, when set, is where catalogs are published, whatever the
	// pipeline's own publishUrl input resolves to. The crawler sets it from
	// its one publishUrl config, so every pipeline it runs -- and every
	// catalog it crawls -- goes to the same provider adapter. Empty leaves
	// the pipeline's input in charge (a standalone run, a test).
	PublishURL string

	// Publisher makes the HTTP call when Publish is set: the crawler passes
	// its sink. Required then -- a run asked to publish with nothing to
	// publish through is refused, not silently built-only.
	Publisher Publisher

	// Config is the calling plugin's own config (the crawler's
	// plugins.<name>.config). Keys `publish.<pipeline>.<input>` override that
	// input, and `publish.<pipeline>.schedule` the file's cron, where
	// <pipeline> is the file's metadata.name -- so an operator changes a
	// pipeline's schedule or identity without a rebuild. Every other key is
	// ignored. Credentials cannot be overridden: they stay in the environment.
	Config map[string]string

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

	// ClaimedElsewhere is true when the firing was due but another replica
	// had already claimed it, so this one did no work. Not a failure.
	ClaimedElsewhere bool

	// Catalogs, Errors and Counters are the run's own result. Errors counts
	// parts of the collection that failed for a real reason; Counters are
	// the file's own numbers (its steps and catalog block record into them),
	// printed but not interpreted.
	Catalogs []BuiltCatalog
	Errors   int
	Counters map[string]int

	OutDir string

	// Published is nil when the run built catalogs without sending them.
	Published *Result
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
	if strings.TrimSpace(opts.Pipeline.URL) == "" {
		return RunReport{}, fmt.Errorf("no pipeline: RunOptions.Pipeline names no URL to run")
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
	spec, err := loadRegistryPipeline(ctx, opts.Pipeline, pipelinePath)
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

	// The operator's schedule, when plugin config gives one, replaces the
	// file's -- a schedule change is then a config edit, not a rebuild. It is
	// parsed by the same due-check below, so a bad one fails loudly here.
	if cron, set := pipelineOverrides(opts.Config, spec.Metadata.Name)[overrideSchedule]; set && strings.TrimSpace(cron) != "" {
		log.InfoContext(ctx, "publish pipeline: schedule from plugin config",
			"pipeline", spec.Metadata.Name, "file", spec.Schedule.Cron, "config", cron)
		spec.Schedule.Cron = strings.TrimSpace(cron)
	}
	// Checked every tick, not only when due, so a mistyped key is reported
	// now rather than at the next firing.
	if _, err := inputOverrides(spec, opts.Config); err != nil {
		return report, err
	}

	// The run log's key is the PIPELINE -- its URL -- not the capability.
	// One capability may have several providers, each its own pipeline file;
	// keyed by capability they would share a row and each would read the
	// other's run as its own.
	key := opts.Pipeline.URL

	// 2. When did it last run. An unreadable log is fatal: "has this firing
	// been served" is then unknown, and the guess that publishes anyway is
	// the one with consequences.
	var lastRun, previous time.Time
	if opts.RunLog != nil {
		if previous, err = opts.RunLog.LastPipelineRun(ctx, key); err != nil {
			return report, fmt.Errorf("reading the run log for %s: %w", key, err)
		}
		lastRun = previous
		// LEGACY, one release: rows used to be keyed by capability, before
		// this run log moved to keying by URL. Read the old row when the
		// pipeline has none, so the first tick after deploy does not run --
		// and publish -- a firing already served. Delete this fallback once
		// production rows have migrated (a database concern, not a code one).
		if lastRun.IsZero() {
			if lastRun, err = opts.RunLog.LastPipelineRun(ctx, capability); err != nil {
				return report, fmt.Errorf("reading the run log for %s: %w", capability, err)
			}
		}
	}

	// 3. Is it due.
	due, firing, reason, err := dueNow(spec.Schedule, now, lastRun)
	if err != nil {
		return report, err
	}
	report.Due, report.Reason = due, reason
	if !due {
		log.InfoContext(ctx, "publish pipeline: not due", "pipeline", key, "reason", reason)
		return report, nil
	}
	if opts.DryRun {
		log.InfoContext(ctx, "publish pipeline: due (dry run, nothing fetched)",
			"pipeline", key, "reason", reason)
		return report, nil
	}

	// 4. Claim the firing BEFORE doing the work. Replicas each see "not run
	// yet"; without a claim each would run the whole pipeline and publish.
	// The claim writes the marker now, conditionally, so exactly one of them
	// gets through and the rest stand down here, having fetched nothing.
	claimer, claims := opts.RunLog.(RunClaimer)
	if claims {
		won, err := claimer.ClaimPipelineRun(ctx, key, now, firing)
		if err != nil {
			return report, fmt.Errorf("claiming %s: %w", key, err)
		}
		if !won {
			report.ClaimedElsewhere = true
			report.Reason = "claimed by another replica: " + reason
			log.InfoContext(ctx, "publish pipeline: firing claimed by another replica; standing down",
				"pipeline", key)
			return report, nil
		}
	}

	// 5. Do the work. A failed run gives the claim back, so a transient
	// outage at midnight is retried on the next tick -- on any replica --
	// rather than costing the whole day.
	if err := execute(ctx, spec, lookup, opts, &report, log, now); err != nil {
		if claims {
			if relErr := claimer.ReleasePipelineRun(ctx, key, previous); relErr != nil {
				log.ErrorContext(ctx, "publish pipeline: run failed and its claim could not be released; "+
					"this firing will not be retried", "pipeline", key, "error", relErr)
			}
		}
		return report, err
	}

	// 6. Only a run that finished is a run that happened. With a claim the
	// marker is already written; recording again is harmless, and it is the
	// only write a run log without claims gets.
	if opts.RunLog != nil {
		if err := opts.RunLog.RecordPipelineRun(ctx, key, now); err != nil {
			// The work is done and published; failing here would make a
			// caller retry a completed run. Loud, but not fatal.
			log.ErrorContext(ctx, "publish pipeline: run finished but could not be recorded; it may run again",
				"pipeline", key, "error", err)
		}
	}
	return report, nil
}

// execute prepares everything the pipeline's steps need, runs them, then
// writes and publishes the catalogs that come back.
func execute(ctx context.Context, spec Spec, lookup func(string) (string, bool),
	opts RunOptions, report *RunReport, log *slog.Logger, now time.Time) error {
	resolved, err := resolveRunInputs(spec, lookup, opts, now)
	if err != nil {
		return err
	}
	var authenticator Authenticator
	if hasUpstream(spec) {
		authenticator, err = authenticatorFor(spec.Upstream.Auth)
		if err != nil {
			return err
		}
		for _, required := range append([]string{inputBaseURL}, authenticator.RequiredInputs()...) {
			if resolved[required] == "" {
				return fmt.Errorf("input %q is empty; the pipeline cannot reach the upstream without it", required)
			}
		}
	}

	// The file's mapping references resolve against its own URL, and
	// jsonmapper fetches them from there -- the same way it loads every
	// other mapping the adapter uses.
	mappingBase := opts.Pipeline.URL

	mapper, closeMapper, err := NewMapper(ctx)
	if err != nil {
		return fmt.Errorf("building the mapper: %w", err)
	}
	defer func() { _ = closeMapper() }()

	// A pipeline with no upstream has nothing to authenticate to and nothing
	// to call: no token, no client. Its steps are const/derive/filter and the
	// like; an HTTP step among them is refused by the schema at load, and by
	// the runner as a backstop.
	offset := offsetIn(spec.Schedule.Timezone, now)
	rc := newRunContext(resolved, "")
	rc.utcOffset = offset
	var client *Client
	if hasUpstream(spec) {
		log.InfoContext(ctx, "publish pipeline: preparing credentials",
			"upstream", resolved[inputBaseURL], "path", spec.Upstream.Auth.Request.Path)
		// The provider's own error classification rides with the client, so
		// what counts as 'nothing here' versus an outage comes from the file
		// rather than from a string this engine happens to know.
		if spec.Upstream.AllowCleartext {
			// Every run, not once at startup: this is a standing exposure,
			// and a warning nobody sees again after deployment is no warning.
			log.WarnContext(ctx, "publish pipeline: upstream.allowCleartext is set — "+
				"credentials and the token travel unencrypted to this upstream",
				"upstream", resolved[inputBaseURL])
		}
		client = NewClient(resolved[inputBaseURL]).
			WithErrorRules(spec.Upstream.Errors).
			WithCleartextAllowed(spec.Upstream.AllowCleartext).
			WithLogger(log)
		cred, err := authenticator.Prepare(ctx, client, rc)
		if err != nil {
			return fmt.Errorf("preparing credentials: %w", err)
		}
		client.WithCredential(cred)
		rc = newRunContext(resolved, cred.Value)
		rc.utcOffset = offset
		log.InfoContext(ctx, "publish pipeline: credentials ready",
			"kind", authKind(spec.Upstream.Auth), "characters", len(cred.Value))
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
	if err := RemoveStaleCatalogs(outDir, prefix); err != nil {
		return fmt.Errorf("clearing previous catalogs: %w", err)
	}

	cache, err := newExprCache()
	if err != nil {
		return err
	}

	// The steps the file declares, in the order it declares them.
	runner := &stepRunner{
		spec: spec, rc: rc, cache: cache, client: client,
		mapper: mapper, mappingBase: mappingBase, log: log,
		counters: map[string]int{}, auth: authenticator,
	}
	log.InfoContext(ctx, "publish pipeline: running steps", "steps", len(spec.Pipeline))
	records, err := runner.runSteps(ctx)
	if err != nil {
		return fmt.Errorf("running the pipeline's steps: %w", err)
	}
	log.InfoContext(ctx, "publish pipeline: steps done", "records", len(records))

	// The catalogs the file's `catalog:` block describes.
	log.InfoContext(ctx, "publish pipeline: building catalogs",
		"groupBy", spec.Catalog.GroupBy, "budget", spec.Catalog.Chunk.Budget)
	catalogs, buildCounters, err := buildCatalogs(ctx, spec.Catalog, records, rc, cache, mapper, mappingBase)
	if err != nil {
		return fmt.Errorf("building catalogs: %w", err)
	}

	// Both halves report into one set of counters, named by the file.
	counters := runner.counters
	for name, count := range buildCounters {
		counters[name] += count
	}
	report.Catalogs = catalogs
	report.Counters = counters
	report.Errors = collectionErrors(spec.Publish, counters)

	if err := WriteCatalogs(catalogs, outDir, prefix); err != nil {
		return fmt.Errorf("writing catalogs: %w", err)
	}
	log.InfoContext(ctx, "publish pipeline: built catalogs",
		"capability", report.Capability, "catalogs", len(catalogs),
		"collectionErrors", report.Errors, "counters", counters, "dir", outDir)

	if !opts.Publish {
		return nil
	}
	log.InfoContext(ctx, "publish pipeline: PUBLISHING to the network",
		"catalogs", len(catalogs), "target", resolved["publishUrl"])
	publishSpec := spec.Publish
	publishSpec.AddressHint = publishAddressHintFor(spec.Inputs["publishUrl"])
	result, err := PublishCatalogs(ctx, publishSpec, resolved, outDir, prefix, counters, opts.Publisher)
	report.Published = &result
	if err != nil {
		return fmt.Errorf("publishing catalogs: %w", err)
	}
	if result.RetiredSkipped != "" {
		// Loud, because the file ASKED for a retirement and did not get one.
		log.WarnContext(ctx, "publish pipeline: the declared retirement was skipped",
			"why", result.RetiredSkipped)
	}
	for _, outcome := range result.Outcomes {
		log.InfoContext(ctx, "publish pipeline: outcome",
			"catalogId", outcome.CatalogID, "status", outcome.Status, "reason", outcome.Reason)
	}
	if result.RetiredOld != nil {
		log.InfoContext(ctx, "publish pipeline: retired",
			"catalogId", result.RetiredOld.CatalogID, "status", result.RetiredOld.Status,
			"reason", result.RetiredOld.Reason)
	}
	if result.HasFailures() {
		return fmt.Errorf("at least one catalog did not reach the network intact")
	}
	return nil
}

// overrideSchedule is the plugin-config key, under publish.<pipeline>., that
// replaces the file's cron.
const overrideSchedule = "schedule"

// pipelineOverrides returns the plugin-config values addressed to this
// pipeline -- keys publish.<metadata.name>.<key> -- keyed by <key>. The name
// scopes them, so two pipelines' baseUrl never collide.
func pipelineOverrides(config map[string]string, name string) map[string]string {
	prefix := "publish." + strings.TrimSpace(name) + "."
	out := map[string]string{}
	for key, value := range config {
		if rest, found := strings.CutPrefix(key, prefix); found && rest != "" {
			out[rest] = value
		}
	}
	return out
}

// inputOverrides is pipelineOverrides less the schedule, checked: every key
// must name an input the file declares -- a typo that matched nothing would
// otherwise be an override an operator believes is in force -- and none may
// be a secret. Credentials stay in the environment: plugin config is read,
// logged and committed.
func inputOverrides(spec Spec, config map[string]string) (map[string]string, error) {
	overrides := pipelineOverrides(config, spec.Metadata.Name)
	delete(overrides, overrideSchedule)

	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		input, declared := spec.Inputs[key]
		if !declared {
			names := make([]string, 0, len(spec.Inputs))
			for name, in := range spec.Inputs {
				if !in.Secret {
					names = append(names, name)
				}
			}
			sort.Strings(names)
			return nil, fmt.Errorf("config publish.%s.%s names no input of this pipeline; it can override %s or %s",
				spec.Metadata.Name, key, strings.Join(names, ", "), overrideSchedule)
		}
		if input.Secret {
			return nil, fmt.Errorf("config publish.%s.%s overrides a secret input; credentials come from the "+
				"environment only, never plugin config", spec.Metadata.Name, key)
		}
	}
	return overrides, nil
}

// resolveRunInputs resolves the file's inputs against the RUN's clock and
// applies the caller's publish address.
//
// The clock is the run's, not the wall's: a test that fixes RunOptions.Now
// must get the dates that instant implies, or every golden file pins the day
// it was generated on.
func resolveRunInputs(spec Spec, lookup func(string) (string, bool), opts RunOptions, now time.Time) (map[string]string, error) {
	overrides, err := inputOverrides(spec, opts.Config)
	if err != nil {
		return nil, err
	}
	resolved, err := resolveInputsWith(spec, lookup, overrides, now)
	if err != nil {
		return nil, fmt.Errorf("resolving pipeline inputs: %w", err)
	}
	if url := strings.TrimSpace(opts.PublishURL); url != "" {
		resolved["publishUrl"] = url
	}
	// Sorted, so two runs of one broken config name the same input first.
	names := make([]string, 0, len(spec.Inputs))
	for name := range spec.Inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		input := spec.Inputs[name]
		if input.Required && strings.TrimSpace(resolved[name]) == "" {
			where := "it has no default"
			if input.Env != "" {
				where = "set " + input.Env
			}
			return nil, fmt.Errorf("input %q is required and resolved to empty (%s)", name, where)
		}
	}
	return resolved, nil
}

// offsetIn renders the schedule zone's UTC offset at now, as +05:30. A
// timestamp published without it reads as UTC and shifts the day for every
// consumer east or west of Greenwich.
func offsetIn(timezone string, now time.Time) string {
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return "" // resolveInputsAt has already refused an unloadable zone
	}
	return now.In(location).Format("-07:00")
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
			return "", nil, fmt.Errorf("making a directory for the catalogs: %w", err)
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
// The rule is `collection.<counter> > <N>`, and <counter> is whatever the
// pipeline's own steps record into -- mandi calls it groupErrors, another
// pipeline will call it something else. Resolving it by name rather than
// hardcoding one keeps the refusal the file's decision.
//
// A file with no refuseWhen has no refusal, and reports zero.
func collectionErrors(spec Publish, counters map[string]int) int {
	counter, _, ok := refuseWhenRule(spec.RefuseWhen)
	if !ok {
		return 0
	}
	return counters[counter]
}

// hasUpstream reports whether the file declares an upstream at all.
//
// The absence of the block is the signal, not an auth kind: a pipeline whose
// catalog is fixed has no service to name, and asking it to invent a
// baseUrl and credentials it never uses would make every such file lie.
func hasUpstream(spec Spec) bool {
	return strings.TrimSpace(spec.Upstream.BaseURL) != ""
}
