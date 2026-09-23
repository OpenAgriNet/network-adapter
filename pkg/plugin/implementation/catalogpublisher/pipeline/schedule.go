package pipeline

// schedule.go answers the two questions a tick has to answer before it runs
// anything: is this capability allowed to publish, and is it due?
//
// Both are deliberately pure decisions over values the caller supplies -- a
// registry record, a clock, the last run's timestamp -- so they can be tested
// against real answers without a scheduler, a database, or a live upstream.
// Wiring them to the crawler's own ticker is a separate, thin step.

import (
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

// publishAction is the action name the registry uses to say "this capability
// publishes a catalogue, and here is the pipeline that does it".
const publishAction = "publish"

// pipelinePathFor is the registry gate: it returns the pipeline definition the
// registry says this capability publishes with, and refuses otherwise.
//
// The path is NOT derived from the capability's name. Deriving it would mean
// guessing a filesystem layout and silently running a pipeline the registry
// never sanctioned; here a capability with no publish action simply has no
// pipeline, which is a fact rather than an error to paper over.
//
// The caller decides what an absent action means. For a capability that is
// only ever consumed (select-only, as every other capability in this network
// is today) it means "nothing to do", not "misconfigured".
func PipelinePathFor(record *model.ProviderRecord) (string, error) {
	if record == nil {
		return "", fmt.Errorf("no provider record: the registry did not confirm this capability")
	}

	plan, ok := record.Actions[publishAction]
	if !ok {
		return "", fmt.Errorf("%s serves no %q action: the registry lists %s, so there is no pipeline to run",
			record.BindingKey, publishAction, servedActions(record))
	}
	path := strings.TrimSpace(plan.Mappings)
	if path == "" {
		return "", fmt.Errorf("%s declares a %q action with no mappings, so it names no pipeline",
			record.BindingKey, publishAction)
	}
	return path, nil
}

// servedActions lists what a record does serve, for an error that says what is
// there rather than only what is missing.
func servedActions(record *model.ProviderRecord) string {
	if len(record.Actions) == 0 {
		return "no actions at all"
	}
	names := make([]string, 0, len(record.Actions))
	for name := range record.Actions {
		names = append(names, strconv.Quote(name))
	}
	return strings.Join(names, ", ")
}

// loadRegistryPipeline loads the pipeline the registry named, for this
// collector.
//
// It refuses any path that is not the collector's own. The registry is data an
// operator edits; a path pointing at some other capability's pipeline must
// read as "that is not mine to run" rather than quietly loading this one and
// publishing one capability's catalogues under another's name. A binary can
// only run what it embeds.
//
// Both spellings are accepted: the repo-relative path an operator pastes into
// a record, and the bare filename, which is what the embedded filesystem knows.
func loadRegistryPipeline(files Files, registryPath string) (Spec, error) {

	cleaned := path.Clean(strings.TrimSpace(registryPath))
	if cleaned == "." || cleaned == "" {
		return Spec{}, fmt.Errorf("the registry names no pipeline path")
	}
	if cleaned != files.Path && cleaned != path.Clean(files.RegistryPath) {
		return Spec{}, fmt.Errorf("the registry names pipeline %q, which is not this pipeline's own %s; "+
			"this binary can only run the pipelines it embeds", cleaned, files.RegistryPath)
	}
	return LoadSpec(files.FS, files.Path)
}

// TickDecision is everything a tick establishes before doing any work: which
// pipeline the registry sanctioned, what it says, and whether it is due.
type TickDecision struct {
	PipelinePath string
	Spec         Spec
	Due          bool
	Reason       string
}

// decideTick runs the pre-flight in order: the registry gate, then the
// pipeline it named, then that pipeline's own schedule.
//
// An error means the tick must not proceed -- the capability does not publish,
// or names a pipeline this binary does not have. A decision with Due false is
// the normal quiet outcome and carries the reason, so an operator asking "why
// did nothing run at 00:05" gets an answer instead of silence.
func decideTick(files Files, record *model.ProviderRecord, now, lastRun time.Time) (TickDecision, error) {
	pipelinePath, err := PipelinePathFor(record)
	if err != nil {
		return TickDecision{}, err
	}
	spec, err := loadRegistryPipeline(files, pipelinePath)
	if err != nil {
		return TickDecision{}, err
	}
	due, reason, err := dueNow(spec.Schedule, now, lastRun)
	if err != nil {
		return TickDecision{}, err
	}
	return TickDecision{PipelinePath: pipelinePath, Spec: spec, Due: due, Reason: reason}, nil
}

// dueNow decides whether a pipeline should run, given when it last ran.
//
// The rule is one line: find the most recent instant the cron expression
// fired, and run if the last run was before it. That covers every cadence a
// cron expression can state -- daily, hourly, weekdays only -- without this
// function knowing which one it is looking at.
//
// The expression is resolved in the pipeline's OWN timezone, not the host's.
// This one fires at midnight Asia/Kolkata; a host running UTC reading its own
// clock would fire on the wrong calendar day and ask the upstream for
// yesterday.
//
// lastRun is passed in rather than read here so this stays a pure decision.
// It must outlive the process -- a restart at 00:05 must not re-fire a run
// that already happened at 00:01 -- which is what the RunLog in run.go is
// for. A zero lastRun means "never ran".
func dueNow(schedule Schedule, now, lastRun time.Time) (bool, string, error) {
	location, err := time.LoadLocation(schedule.Timezone)
	if err != nil {
		return false, "", fmt.Errorf("schedule.timezone %q: %w", schedule.Timezone, err)
	}
	expr, err := parseCron(schedule.Cron)
	if err != nil {
		return false, "", fmt.Errorf("schedule.cron: %w", err)
	}

	firing, err := expr.prevFiring(now, location)
	if err != nil {
		return false, "", fmt.Errorf("schedule.cron: %w", err)
	}
	stamp := firing.Format("2006-01-02 15:04 MST")

	if lastRun.IsZero() {
		return true, fmt.Sprintf("due: %q last fired %s and this pipeline has never run", schedule.Cron, stamp), nil
	}
	if lastRun.Before(firing) {
		return true, fmt.Sprintf("due: %q fired %s, after the last run at %s",
			schedule.Cron, stamp, lastRun.In(location).Format("2006-01-02 15:04 MST")), nil
	}
	return false, fmt.Sprintf("not due: already ran at %s, at or after the %s firing",
		lastRun.In(location).Format("2006-01-02 15:04 MST"), stamp), nil
}
