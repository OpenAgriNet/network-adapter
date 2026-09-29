// Package pipeline is the shared frame for scheduled publish pipelines: the
// registry gate, the cron schedule, the run log, input resolution, the
// upstream client, and the publish step. Everything in it is the same for
// every pipeline; what differs -- what to fetch, how to shape it, what a
// catalog of it contains -- is declared in the pipeline's own YAML (interpreter.go
// interprets the step list; the catalog-building code below turns the result
// into documents), and this package never hardcodes a capability.
//
// It must NOT be moved under an internal/ directory: the crawler that ticks a
// pipeline imports it, and so do the capability folders' tests.
//
// A pipeline is a hosted file: the registry's publish action names its https
// URL, and a run fetches it (source.go), exactly as the adapter fetches every
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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
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

// releaseTimeout bounds the claim-release call a failed run makes on its way
// out. Short and decoupled from the run's own context on purpose: this call
// runs BECAUSE something -- often the context itself -- already went wrong,
// and it is one small database write, not a network call to an upstream.
const releaseTimeout = 5 * time.Second

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
			// Decoupled from ctx and given its own short budget: ctx is very
			// often the reason execute failed (a shutdown, a deadline), and
			// reusing it here would fail the release the same way, leaving
			// the claim marker set to "now" -- so dueNow reads this firing as
			// already handled until the NEXT scheduled one, silently losing
			// it for the rest of the window.
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
			relErr := claimer.ReleasePipelineRun(releaseCtx, key, previous)
			cancel()
			if relErr != nil {
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
	offset := offsetIn(spec.Schedule.UTCOffset, now)
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

	// WriteCatalogs clears the previous run's files before writing, so the
	// last good catalogs survive until there is a complete set to replace
	// them -- and are never cleared by a run that was cancelled.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("building catalogs: %w", err)
	}
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

// offsetIn renders schedule.utcOffset in the canonical "+05:30" form. A
// timestamp published without it reads as UTC and shifts the day for every
// consumer east or west of Greenwich.
func offsetIn(utcOffset string, now time.Time) string {
	location, err := parseUTCOffset(utcOffset)
	if err != nil {
		return "" // resolveInputsAt has already refused an unparseable offset
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

// ---------------------------------------------------------------------------
// catalog: building the documents this run publishes.
//
// This is the generalisation of MandiPrice's original catalog.go, whose
// behaviour it preserves rule for rule -- the geometry budget, the two
// exclusions, the annotation, the refusal to publish an empty group -- with
// every one of them read from the file instead of written in Go. Nothing here
// knows what a mandi, a market or a state is: a second capability gets a
// catalog by writing a `catalog:` block and no Go at all.
// ---------------------------------------------------------------------------

// Counter names the builder reports under. Prefixed keys carry the file's own
// wording -- "excluded:coordinate missing" -- because a bare total tells an
// operator that something was dropped but never which rule dropped it.
const (
	catalogCounterGroups    = "groups"
	catalogCounterEmpty     = "excludedGroups" // a group every member of which was excluded
	catalogCounterCatalogs  = "catalogs"
	catalogCounterPublished = "published"
	catalogCounterExcluded  = "excluded"

	catalogExcludedPrefix  = "excluded:"
	catalogAnnotatedPrefix = "annotated:"
)

// catalogResourceField is where identity.resourceId lands on each record
// handed to the render mapping.
//
// The template has to DO something. A file that states the id its resources
// publish under, while the mapping quietly builds its own, is the failure
// spec.go's header warns about: a rule believed to be in force that no code
// ever reads.
const catalogResourceField = "resourceId"

// buildCatalogs applies a file's `catalog:` block to a collection.
//
// rc supplies everything a `${...}` outside a record can name (the resolved
// inputs, the built-ins); cache compiles and applies the JSONata that reaches
// inside one. mappingBase is the pipeline file's URL, which a mapping
// reference resolves against, as
// interpreter.go's mappingRef uses it.
//
// The counters are the run's own report numbers, handed back rather than
// logged so RunReport.Counters carries them.
func buildCatalogs(ctx context.Context, catalog Catalog, records []map[string]any,
	rc *runContext, cache *exprCache, mapper Mapper, mappingBase string) ([]BuiltCatalog, map[string]int, error) {

	counters := map[string]int{}

	if catalog.GroupBy == "" {
		return nil, counters, fmt.Errorf("the catalog block names no groupBy field")
	}
	if catalog.Render.Mapping == "" {
		return nil, counters, fmt.Errorf("the catalog block names no render mapping")
	}
	if catalog.Identity.CatalogID == "" {
		return nil, counters, fmt.Errorf("the catalog block names no identity.catalogId: a catalog with no id cannot be published")
	}
	if err := catalogCheckDirection(catalog.Order.Direction); err != nil {
		return nil, counters, err
	}

	keys, groups, err := catalogGroup(records, catalog.GroupBy)
	if err != nil {
		return nil, counters, err
	}
	counters[catalogCounterGroups] = len(keys)

	var built []BuiltCatalog
	named := map[string]string{}

	for _, key := range keys {
		members := groups[key]

		// The group's own values, reachable as ${group.…}: for each field, the
		// first value any member carries that is not blank. Taken from the
		// first member alone, a descriptor built from ${group.stateName} would
		// be named after whichever row happened to sort first, blank included.
		scope := rc.with(catalog.GroupBy, key).with("group", catalogGroupValues(members))

		publishable, err := catalogSelect(catalog, members, scope, cache, counters)
		if err != nil {
			return nil, counters, fmt.Errorf("group %q: %w", key, err)
		}

		// A group left with nothing publishable produces NO catalog, not an
		// empty one. An empty catalog is not "no news": publishing it
		// retires that group's resources from the network on the next MERGE.
		if len(publishable) == 0 {
			counters[catalogCounterEmpty]++
			continue
		}

		if catalog.Order.By != "" {
			sortRecords(publishable, catalog.Order.By, catalog.Order.Direction)
		}

		chunks, err := catalogChunk(publishable, catalog.Chunk, cache)
		if err != nil {
			return nil, counters, fmt.Errorf("group %q: %w", key, err)
		}

		for index, chunk := range chunks {
			// The split is settled; now the order a reader sees. Sorting a
			// chunk cannot move a record between chunks.
			if len(catalog.Order.RenderBy) > 0 {
				sortRecordsBy(chunk, catalog.Order.RenderBy)
			}
			catalog, err := catalogRender(ctx, catalog, chunk, index+1, key, scope, cache, mapper, mappingBase)
			if err != nil {
				return nil, counters, fmt.Errorf("group %q: %w", key, err)
			}
			// Two catalogs under one slug are two catalogs under one
			// catalogId: the second MERGE would replace the first one's
			// resources instead of adding to them, and the second file would
			// overwrite the first on disk.
			// Compared case-FOLDED, because the filesystem may be: on macOS
			// and Windows "mh" and "MH" are one file, so two groups whose
			// names differ only in case would silently overwrite each other
			// and publish one catalog under two ids.
			folded := strings.ToLower(catalog.Slug)
			if previous, taken := named[folded]; taken {
				// The two mistakes look identical from here and are fixed in
				// different places, so the message has to say which one it is:
				// an exact repeat is a chunk template that ignores the chunk
				// index, while a folded one is two group names.
				if previous == catalog.Slug {
					return nil, counters, fmt.Errorf("group %q: chunk %d renders the slug %q again; "+
						"catalog.chunk.slug must include the chunk index, or every chunk of a split "+
						"group is one catalogId and one file",
						key, index+1, catalog.Slug)
				}
				return nil, counters, fmt.Errorf("group %q: chunk %d renders the slug %q, which collides with %q; "+
					"they differ only in case and are one file on a case-insensitive filesystem",
					key, index+1, catalog.Slug, previous)
			}
			named[folded] = catalog.Slug

			built = append(built, catalog)
			counters[catalogCounterCatalogs]++
			counters[catalogCounterPublished] += len(chunk)
		}
	}

	return built, counters, nil
}

// catalogGroup splits the collection on one field, returning the keys in
// sorted order so a run partitions the same way twice.
//
// A record that does not carry the field is an ERROR. Grouped under the empty
// key it would publish as a catalog named after nothing, and a mistyped
// groupBy would collapse an entire collection into one such catalog.
func catalogGroup(records []map[string]any, by string) ([]string, map[string][]map[string]any, error) {
	groups := map[string][]map[string]any{}
	var keys []string

	for i, record := range records {
		value, ok := record[by]
		if !ok {
			return nil, nil, fmt.Errorf("record %d has no %q to group by", i, by)
		}
		key := renderScalar(value)
		if _, seen := groups[key]; !seen {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], record)
	}

	sort.Strings(keys)
	return keys, groups, nil
}

// catalogGroupValues is what ${group.…} resolves against: for every field any
// member carries, the first non-blank value in collection order.
func catalogGroupValues(members []map[string]any) map[string]any {
	values := map[string]any{}
	for _, member := range members {
		for field, value := range member {
			if existing, seen := values[field]; seen && !catalogBlank(existing) {
				continue
			}
			values[field] = value
		}
	}
	return values
}

func catalogBlank(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(typed) == ""
	default:
		return false
	}
}

// catalogSelect applies the exclusions and then the annotations, in that
// order: a record kept out of the catalog is not a record to label.
//
// The records it returns are COPIES. An annotation writing onto the caller's
// collection would leave the previous run's labels on a reused record.
func catalogSelect(catalog Catalog, members []map[string]any, scope *runContext,
	cache *exprCache, counters map[string]int) ([]map[string]any, error) {

	var publishable []map[string]any

	for _, record := range members {
		excluded, err := catalogExcluded(catalog.Exclude, record, scope, cache, counters)
		if err != nil {
			return nil, err
		}
		if excluded {
			continue
		}

		kept := cloneRecord(record)
		for i, rule := range catalog.Annotate {
			if rule.As == "" {
				return nil, fmt.Errorf("annotate rule %d says no `as:` field to set", i)
			}
			if rule.When == "" {
				return nil, fmt.Errorf("annotate rule %d has no `when:`", i)
			}
			// Evaluated against the record as it arrived, so one rule cannot
			// fire because an earlier one just labelled the record.
			match, err := catalogMatches(cache, scope, rule.When, record)
			if err != nil {
				return nil, fmt.Errorf("annotate rule %d: %w", i, err)
			}
			if match {
				kept[rule.As] = true
				counters[catalogAnnotatedPrefix+rule.As]++
			}
		}
		publishable = append(publishable, kept)
	}

	return publishable, nil
}

// catalogExcluded reports whether any exclusion rule claims this record, and
// counts the claim under the rule's own reason.
func catalogExcluded(rules []ExcludeRule, record map[string]any, scope *runContext,
	cache *exprCache, counters map[string]int) (bool, error) {

	for i, rule := range rules {
		if rule.When == "" {
			return false, fmt.Errorf("exclude rule %d has no `when:`", i)
		}
		if rule.Reason == "" {
			// Every excluded record is reported with a stated reason. A
			// silent exclusion is a resource that vanishes from the network
			// and is indistinguishable from one that never existed.
			return false, fmt.Errorf("exclude rule %d states no `reason:`", i)
		}
		match, err := catalogMatches(cache, scope, rule.When, record)
		if err != nil {
			return false, fmt.Errorf("exclude rule %d: %w", i, err)
		}
		if !match {
			continue
		}
		// The reason is interpolated against the record, so a rule covering
		// several defects ("coordinate ${coordinateQuality}") reports each of
		// them by name rather than lumping them together.
		reason, err := catalogRecordScope(scope, record).interpolate(rule.Reason)
		if err != nil {
			return false, fmt.Errorf("exclude rule %d reason: %w", i, err)
		}
		counters[catalogCounterExcluded]++
		counters[catalogExcludedPrefix+reason]++
		return true, nil
	}
	return false, nil
}

// catalogMatches applies one rule's `when:` to one record.
//
// An expression that fails to evaluate is an error, never false: a predicate
// that silently reads as false is how an exclusion rule stops excluding with
// nothing to see.
func catalogMatches(cache *exprCache, scope *runContext, when string, record map[string]any) (bool, error) {
	expr, err := interpolatePredicate(scope, when)
	if err != nil {
		return false, err
	}
	return cache.truthy(expr, record)
}

// catalogRecordScope puts a record's own fields within reach of `${...}`, so
// a reason or an id template can name them: `coordinate ${coordinateQuality}`,
// `resource:mandi-price:market:${marketId}`.
func catalogRecordScope(scope *runContext, record map[string]any) *runContext {
	// Sized by the scope alone: summing two lengths for a capacity hint is an
	// addition that can overflow in principle (CodeQL go/allocation-size-
	// overflow), and the map grows as needed anyway.
	locals := make(map[string]any, len(scope.locals))
	for name, value := range scope.locals {
		locals[name] = value
	}
	for name, value := range record {
		locals[name] = value
	}
	return &runContext{inputs: scope.inputs, token: scope.token, outputs: scope.outputs, locals: locals, utcOffset: scope.utcOffset}
}

func catalogCheckDirection(direction string) error {
	switch {
	case direction == "",
		strings.EqualFold(direction, "asc"),
		strings.EqualFold(direction, "desc"):
		return nil
	default:
		return fmt.Errorf("order direction %q is not supported; use asc or desc", direction)
	}
}

// catalogChunk splits a group into runs that each stay within the budget.
//
// A record costing nothing never forces a split; a run is cut only when adding
// the NEXT record would exceed the budget, so exactly budget-many costing
// records still make one catalog. Order is preserved, so the partition is
// deterministic and a record lands in exactly one chunk.
//
// A budget of zero or less means the group is not split at all -- a catalog
// with no declared limit is one catalog, not an unbounded number of them.
func catalogChunk(records []map[string]any, spec Chunk, cache *exprCache) ([][]map[string]any, error) {
	if spec.Budget <= 0 {
		return [][]map[string]any{records}, nil
	}

	var chunks [][]map[string]any
	var current []map[string]any
	spent := 0

	for _, record := range records {
		cost, err := catalogCost(spec.Cost, record, cache)
		if err != nil {
			return nil, err
		}
		if len(current) > 0 && spent+cost > spec.Budget {
			chunks = append(chunks, current)
			current, spent = nil, 0
		}
		current = append(current, record)
		spent += cost
	}
	if len(current) > 0 {
		chunks = append(chunks, current)
	}
	return chunks, nil
}

// catalogCost is what one record spends of the budget. An absent `cost:`
// means one per record, which is chunking by count.
func catalogCost(expr string, record map[string]any, cache *exprCache) (int, error) {
	if strings.TrimSpace(expr) == "" {
		return 1, nil
	}
	value, err := cache.evaluate(expr, record)
	if err != nil {
		return 0, fmt.Errorf("chunk cost: %w", err)
	}
	number, ok := numeric(value)
	if !ok {
		// A cost that is not a number would otherwise count as zero and the
		// budget would never be reached, which looks exactly like a group that
		// fits.
		return 0, fmt.Errorf("chunk cost %q produced %v, which is not a number", expr, value)
	}
	if number < 0 {
		return 0, fmt.Errorf("chunk cost %q produced %v: a negative cost would buy back budget already spent", expr, number)
	}
	return int(number), nil
}

// catalogRender names one chunk and turns it into a document.
func catalogRender(ctx context.Context, catalog Catalog, chunk []map[string]any,
	index int, key string, scope *runContext, cache *exprCache,
	mapper Mapper, mappingBase string) (BuiltCatalog, error) {

	slug, err := catalogSlug(catalog, index, key, scope, cache)
	if err != nil {
		return BuiltCatalog{}, err
	}

	chunkScope := scope.with("slug", slug).with("chunkIndex", index)

	catalogID, err := chunkScope.interpolate(catalog.Identity.CatalogID)
	if err != nil {
		return BuiltCatalog{}, fmt.Errorf("identity.catalogId: %w", err)
	}

	if catalog.Identity.ResourceID != "" {
		for _, record := range chunk {
			resourceID, err := catalogRecordScope(chunkScope, record).interpolate(catalog.Identity.ResourceID)
			if err != nil {
				return BuiltCatalog{}, fmt.Errorf("identity.resourceId: %w", err)
			}
			record[catalogResourceField] = resourceID
		}
	}

	local := make(map[string]any, len(catalog.Render.Local))
	for name, template := range catalog.Render.Local {
		value, err := chunkScope.interpolate(template)
		if err != nil {
			return BuiltCatalog{}, fmt.Errorf("render local %q: %w", name, err)
		}
		local[name] = value
	}

	// The catalog's id is the engine's, rendered once from identity.catalogId.
	// Handing it to the mapping means the document and the file cannot
	// disagree on it, and the id format is written in one place, the
	// pipeline file. A render local of the same name is overwritten.
	local["catalogId"] = catalogID

	// The same input shape the mapping half of every other call receives: the
	// records under `response`, everything the payload does not carry under
	// `_local`.
	input := map[string]any{"response": chunk, "_local": local}

	// Resolved against the pipeline file's URL, as interpreter.go's mappingRef.
	ref, err := resolveMappingRef(mappingBase, catalog.Render.Mapping)
	if err != nil {
		return BuiltCatalog{}, err
	}

	content, err := mapper.Transform(ctx, ref, definition.DirectionResponse, input)
	if err != nil {
		return BuiltCatalog{}, fmt.Errorf("rendering %s: %w", slug, err)
	}

	return BuiltCatalog{Slug: slug, CatalogID: catalogID, Content: content}, nil
}

// catalogSlug renders the chunk's name.
//
// Each ${...} in the template is a JSONata expression, not a dotted path,
// because the one this file has to support is a ternary:
//
//	slug: "${stateCode}${chunkIndex > 1 ? '-' & chunkIndex : ''}"
//
// which keeps a group small enough to fit under its plain name and numbers
// only the second and later chunks -- so a group's catalogId never changes
// merely because another group grew.
//
// The expressions are evaluated against the group's own values plus
// chunkIndex and inputs, and nothing else: a slug is a name for this group,
// and one reaching into another step's output would name a file after
// something that has nothing to do with what is in it. An expression that
// resolves to nothing is an error, not an empty piece of name.
func catalogSlug(catalog Catalog, index int, key string, scope *runContext, cache *exprCache) (string, error) {
	if catalog.Chunk.Slug == "" {
		// No template: the group's own key is its name -- still checked,
		// because a group key is upstream data too.
		return key, safeSlug(key, "the group key")
	}

	document := map[string]any{}
	if group, ok := scope.locals["group"].(map[string]any); ok {
		for field, value := range group {
			document[field] = value
		}
	}
	document[catalog.GroupBy] = key
	document["chunkIndex"] = index
	document["inputs"] = scope.inputs

	var failed error
	out := placeholder.ReplaceAllStringFunc(catalog.Chunk.Slug, func(match string) string {
		expr := strings.TrimSpace(placeholder.FindStringSubmatch(match)[1])
		value, err := cache.evaluate(expr, document)
		if err != nil {
			if failed == nil {
				failed = fmt.Errorf("chunk slug: %w", err)
			}
			return match
		}
		switch value.(type) {
		case nil:
			if failed == nil {
				failed = fmt.Errorf("chunk slug: %q resolved to nothing", expr)
			}
			return match
		case map[string]any, []any:
			if failed == nil {
				failed = fmt.Errorf("chunk slug: %q produced a %T, which is not a name", expr, value)
			}
			return match
		}
		return renderScalar(value)
	})
	if failed != nil {
		return "", failed
	}
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("chunk slug %q rendered empty: a catalog with no name cannot be written or published", catalog.Chunk.Slug)
	}
	if err := safeSlug(out, fmt.Sprintf("chunk slug %q", catalog.Chunk.Slug)); err != nil {
		return "", err
	}
	return out, nil
}

// WriteCatalogs writes each built catalog to <dir>/<filenamePrefix>-<slug>.json,
// after clearing any catalog this run did not produce.
//
// Indented, because these files exist to be read: somebody reviews what is
// about to go onto the network, and a single-line document of several hundred
// markets cannot be reviewed. The publish step reads the same directory back,
// which is why the naming is fixed rather than a caller's choice.
//
// THE CLEARING IS NOT HOUSEKEEPING. The publish step globs every
// <filenamePrefix>-*.json in this directory and posts all of them, so a
// catalog left behind by an earlier run is republished as though it were
// current. Maharashtra splitting into MH and MH-2 on Monday and fitting into
// one catalog on Tuesday would, without this, republish Monday's MH-2 on
// Tuesday: a document of markets that no longer belong in one. A human
// running this by hand could see the directory and notice; the daily
// unattended loop this package exists for cannot, and the bug is invisible on
// the first run and wrong on every one after it.
func WriteCatalogs(built []BuiltCatalog, dir, filenamePrefix string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create catalog output directory: %w", err)
	}

	if err := RemoveStaleCatalogs(dir, filenamePrefix); err != nil {
		return err
	}

	for _, catalog := range built {
		var indented bytes.Buffer
		if err := json.Indent(&indented, catalog.Content, "", "  "); err != nil {
			return fmt.Errorf("indent JSON for catalog %s: %w", catalog.Slug, err)
		}

		path := filepath.Join(dir, fmt.Sprintf("%s-%s.json", filenamePrefix, catalog.Slug))
		if err := os.WriteFile(path, indented.Bytes(), 0o644); err != nil {
			return fmt.Errorf("write catalog file %s: %w", path, err)
		}
	}
	return nil
}

// RemoveStaleCatalogs deletes the catalogs already in dir, so only this run's
// output is left for the publish step to find.
//
// The match is deliberately the SAME one catalogFiles (publish.go) makes --
// a non-directory entry whose name starts with "<filenamePrefix>-" and ends
// in ".json" -- because the set this removes has to be exactly the set that
// would otherwise be published. Matching more broadly would delete a file
// somebody else put here; matching more narrowly would leave one that still
// gets posted.
//
// Nothing else is touched: no recursion, no directories, and no file outside
// that pattern. This directory is an operator's to point wherever they like,
// and it may hold things that are not ours.
func RemoveStaleCatalogs(dir, filenamePrefix string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read catalog output directory: %w", err)
	}

	namePrefix := filenamePrefix + "-"
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, namePrefix) || !strings.HasSuffix(name, ".json") {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("remove stale catalog %s: %w", name, err)
		}
	}
	return nil
}

// slugShape is what a catalog name may contain.
//
// Deliberately narrow, because a slug is UPSTREAM DATA that becomes a
// filename, and three separate things go wrong when it is not:
//
//   - "../../x" escapes the output directory. filepath.Join does not save us:
//     "<prefix>-.." is an ordinary path element, so enough "../" segments walk
//     out of the directory the operator chose. Banning the SLASH is what stops
//     this; a bare ".." is harmless, since it only ever lands inside the single
//     filename "<prefix>-...json".
//   - "J/K" writes into a subdirectory, where the publisher's
//     "<prefix>-*.json" glob never finds it. The catalog is built, reported
//     as built, and silently never published.
//   - "mh" and "MH" are the same file on a case-insensitive filesystem, so
//     one group's catalog quietly overwrites another's.
//
// Refused rather than sanitised: rewriting two different upstream values into
// one safe name would merge two groups' catalogs, which is worse than
// stopping.
var slugShape = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// safeSlug refuses a name that cannot safely become a file.
func safeSlug(slug, source string) error {
	if slugShape.MatchString(slug) {
		return nil
	}
	return fmt.Errorf("%s produced %q, which cannot be used as a catalog name: "+
		"a name becomes a filename and must match %s. A slash is the danger -- it both "+
		"writes outside the directory the publisher globs (so the catalog is built and "+
		"never published) and, with enough ../ segments, outside the output directory "+
		"entirely", source, slug, slugShape)
}
