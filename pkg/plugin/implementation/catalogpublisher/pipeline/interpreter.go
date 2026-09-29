package pipeline

// interpreter.go runs the `pipeline:` block: the ordered list of steps a file
// declares, each producing a named output the next ones can read.
//
// Eight primitives:
//
//	http.get    call the upstream via GET, shaping request and response
//	            through a JSONata mapping
//	http.post   call the upstream via POST, building a JSON request body
//	            from the mapping's request half
//	join        match one collection against another on a key
//	derive      add a computed field, by rules, with optional follow-up edits
//	dedupe      collapse repeats by key
//	filter      keep only records matching a JSONata predicate
//	transform   apply a JSONata mapping in-pipeline (no HTTP call)
//	const       emit the records the file itself declares (no upstream)
//
// Everything a step can vary -- the path, the mapping, the loop, what counts
// as a tolerable failure -- comes from the file. Nothing here knows what a
// mandi is.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

// Step primitives, as a file spells them in `uses:`.
const (
	usesHTTPGet   = "http.get"
	usesHTTPPost  = "http.post"
	usesJoin      = "join"
	usesDerive    = "derive"
	usesDedupe    = "dedupe"
	usesFilter    = "filter"
	usesTransform = "transform"
	usesConst     = "const"
)

// maxReauthsPerRun caps re-exchanges in one run. Three covers a token that
// expires mid-run, even more than once; an upstream rejecting every fresh
// token is not going to accept the fourth.
const maxReauthsPerRun = 3

// stepRunner carries what every step needs.
type stepRunner struct {
	spec        Spec
	rc          *runContext
	cache       *exprCache
	client      *Client
	mapper      Mapper
	mappingBase string
	log         *slog.Logger

	// counters are what `onError`/`onEmptyOutput` record into, and what the
	// run reports. The file names them; this code only counts.
	counters map[string]int

	// auth re-obtains the credential when the upstream rejects it. nil for a
	// pipeline with no upstream.
	auth Authenticator

	// reauths counts re-exchanges this run. Capped: an upstream that rejects
	// every fresh token would otherwise be hammered once per call.
	reauths int

	// lastOutput is what the step just before this one produced, whether or
	// not it named itself with `out:`.
	//
	// This used to be found by scanning the pipeline for the last step that
	// named an output, which silently DISCARDED the work of any step that did
	// not. A filter with no `out:` followed by a dedupe is the ordinary shape
	// that hits it: the filter runs, the dedupe reads the collection from
	// before it, and the rows the file excluded are published anyway.
	lastOutput any
}

// runSteps executes the declared steps in order and returns the output of the
// last one, which is the collection the catalog is built from.
func (r *stepRunner) runSteps(ctx context.Context) ([]map[string]any, error) {
	if len(r.spec.Pipeline) == 0 {
		return nil, fmt.Errorf("the pipeline declares no steps")
	}

	var last any
	for i, step := range r.spec.Pipeline {
		// Logged per step, at INFO, because a run is minutes long and mostly
		// silent: without this the only signals are "started" and "finished",
		// and a pipeline stuck on state 19 of 36 looks exactly like one that
		// is merely slow.
		r.log.InfoContext(ctx, "pipeline step: start",
			"step", fmt.Sprintf("%d/%d", i+1, len(r.spec.Pipeline)),
			"id", step.ID, "uses", step.Uses)

		began := time.Now()
		result, err := r.runStep(ctx, step)
		if err != nil {
			return nil, fmt.Errorf("step %q: %w", step.ID, err)
		}
		output := result.value

		// A passed-through value belongs to an EARLIER step. It flows on, so
		// the next implicit consumer still has a collection, but nothing here
		// may attribute it to this one: binding it to this step's `out:` makes
		// ${enriched} resolve to un-enriched data, and counting it makes a
		// step that never ran look like one that produced records.
		if result.passedThrough {
			r.log.InfoContext(ctx, "pipeline step: skipped",
				"step", fmt.Sprintf("%d/%d", i+1, len(r.spec.Pipeline)),
				"id", step.ID, "when", step.When)
			r.lastOutput = output
			last = output
			continue
		}

		produced := recordsForLog(output)
		fields := []any{"step", fmt.Sprintf("%d/%d", i+1, len(r.spec.Pipeline)),
			"id", step.ID, "records", produced.count,
			"took", time.Since(began).Round(time.Millisecond).String()}
		if produced.rawType != "" {
			// Not a collection. Said, rather than reported as zero records,
			// which reads as "the upstream had nothing".
			fields = append(fields, "type", produced.rawType)
		}
		r.log.InfoContext(ctx, "pipeline step: done", fields...)
		if step.Out != "" {
			r.rc.outputs[step.Out] = output
		}
		r.lastOutput = output

		// How much each step produced, without the file having to ask. This
		// is what an operator reads to tell "36 states, 4172 markets" from
		// "36 states, 0 markets" -- two runs that otherwise both succeed and
		// publish nothing useful.
		if records, err := asRecords(output); err == nil {
			r.counters["step:"+step.ID] = len(records)
		}

		last = output
	}

	records, err := asRecords(last)
	if err != nil {
		return nil, fmt.Errorf("the last step did not produce a collection: %w", err)
	}
	return records, nil
}

// stepResult is what one step produced, and whether the step produced it.
type stepResult struct {
	value any

	// passedThrough marks a value the step did not produce: it was skipped
	// with no `else:`, so this is the previous step's collection flowing past.
	passedThrough bool
}

// runStep applies one step's control flow -- the conditional, the loop -- and
// dispatches the primitive underneath.
func (r *stepRunner) runStep(ctx context.Context, step Step) (stepResult, error) {
	// A step's `when:` decides whether it runs at all; `else: const:` supplies
	// what its output is when it does not.
	if step.When != "" {
		run, err := r.condition(step.When)
		if err != nil {
			return stepResult{}, err
		}
		if !run {
			value, err := r.elseValue(step)
			if err != nil {
				return stepResult{}, err
			}
			// A step skipped with no `else:` is a NO-OP, not a step that
			// produced nothing: the collection flows past it untouched. The
			// alternative -- nil -- makes the next implicit consumer fail
			// with "no step before it produced one", which describes the
			// runner's bookkeeping rather than anything the file did.
			if value == nil && strings.TrimSpace(step.Else.Const) == "" {
				return stepResult{value: r.lastOutput, passedThrough: true}, nil
			}
			// An `else:` IS this step's output: the file declared what the
			// step produces when it does not run.
			return stepResult{value: value}, nil
		}
	}

	output, err := r.dispatch(ctx, step)
	if err != nil {
		return stepResult{}, err
	}

	if step.FailWhenEmpty != "" && isEmpty(output) {
		// Not a warning. An empty collection walks cleanly through every
		// later step and produces a well-formed, zero-error, empty result --
		// a run that reads as "there is nothing to publish" and exits zero.
		return stepResult{}, fmt.Errorf("%s", step.FailWhenEmpty)
	}
	return stepResult{value: output}, nil
}

// dispatch runs the primitive, looping it when the step declares forEach.
func (r *stepRunner) dispatch(ctx context.Context, step Step) (any, error) {
	if step.ForEach == "" {
		return r.primitive(ctx, step, r.rc)
	}

	// Concurrency above 1 is refused rather than silently ignored.
	//
	// It cannot be honoured safely today: the JSONata library keeps its
	// built-ins in package-level state, so a parallel iteration would run
	// mapping transforms (jsonmapper's lock) and this package's own
	// evaluations (expressions.go's lock) at the same time over that shared state --
	// two locks, one race. Accepting the key and running sequentially would
	// be worse: the file would state a parallelism that never happens.
	if step.Concurrency > 1 {
		return nil, fmt.Errorf("concurrency: %d is not supported; the JSONata evaluator this "+
			"pipeline shares is not safe to run in parallel, so only 1 is honoured", step.Concurrency)
	}
	if step.As == "" {
		return nil, fmt.Errorf("forEach needs `as:` to name the loop variable")
	}

	items, err := r.collection(step.ForEach)
	if err != nil {
		return nil, err
	}

	var gathered []map[string]any
	for i, item := range items {
		iteration := r.rc.with(step.As, item)
		r.log.DebugContext(ctx, "pipeline step: iteration",
			"id", step.ID, "item", fmt.Sprintf("%d/%d", i+1, len(items)), "as", step.As)

		output, err := r.primitive(withLogItem(ctx, itemLabel(item)), step, iteration)
		if err != nil {
			outcome, matched := r.outcomeFor(step, err)
			if !matched {
				return nil, err
			}
			r.record(outcome, step, item, err)
			if outcome.Continue {
				continue
			}
			return nil, err
		}

		if isEmpty(output) {
			// An empty result is a fact about this item, not a failure -- and
			// the file says what to call it.
			if step.OnEmptyOutput.Record != "" {
				r.record(step.OnEmptyOutput, step, item, nil)
			}
			continue
		}

		records, err := asRecords(output)
		if err != nil {
			return nil, err
		}
		gathered = append(gathered, records...)
	}
	return gathered, nil
}

// primitive runs the operation named by `uses:`.
func (r *stepRunner) primitive(ctx context.Context, step Step, rc *runContext) (any, error) {
	switch step.Uses {
	case usesHTTPGet:
		return r.withReauth(ctx, rc, func(rc *runContext) (any, error) { return r.httpGet(ctx, step, rc) })
	case usesHTTPPost:
		return r.withReauth(ctx, rc, func(rc *runContext) (any, error) { return r.httpPost(ctx, step, rc) })
	case usesJoin:
		return r.join(step, rc)
	case usesDerive:
		return r.derive(step, rc)
	case usesDedupe:
		return r.dedupe(step, rc)
	case usesFilter:
		return r.filter(step, rc)
	case usesTransform:
		return r.transform(ctx, step, rc)
	case usesConst:
		return r.constRecords(step)
	default:
		return nil, fmt.Errorf("uses: %q is not a primitive this runner implements (%s)",
			step.Uses, strings.Join([]string{usesHTTPGet, usesHTTPPost, usesJoin, usesDerive, usesDedupe, usesFilter, usesTransform, usesConst}, ", "))
	}
}

// httpGet calls the upstream through the step's mapping.
func (r *stepRunner) httpGet(ctx context.Context, step Step, rc *runContext) (any, error) {
	if r.client == nil {
		return nil, errNoUpstream(step)
	}
	if step.With.Path == "" || step.With.Mapping == "" {
		return nil, fmt.Errorf("http.get needs both `path:` and `mapping:`")
	}
	path, err := rc.interpolate(step.With.Path)
	if err != nil {
		return nil, err
	}

	local := make(map[string]any, len(step.With.Local))
	for key, template := range step.With.Local {
		value, err := rc.interpolate(template)
		if err != nil {
			return nil, fmt.Errorf("local.%s: %w", key, err)
		}
		local[key] = value
	}

	ref, err := r.mappingRef(step.With.Mapping)
	if err != nil {
		return nil, err
	}
	raw, err := r.client.Get(ctx, r.mapper, ref, path, local)
	if err != nil {
		return nil, err
	}

	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("the mapped response did not decode: %w", err)
	}
	return decoded, nil
}

// mappingRef turns a file-relative mapping path into the URL the mapper
// fetches.
func (r *stepRunner) mappingRef(mapping string) (string, error) {
	// Resolved against the pipeline file's URL, the way a link in a page is
	// resolved against the page: `mappings/master-states.yaml` beside the
	// file. url.ResolveReference, not string joins -- filepath.Join collapses
	// the "//" in "https://host".
	return resolveMappingRef(r.mappingBase, mapping)
}

// join matches left against right on a shared key, carrying named fields
// across. A left row with no match is KEPT -- it is a real record whose extra
// fields are merely unknown.
func (r *stepRunner) join(step Step, rc *runContext) (any, error) {
	if step.With.Type != "" && step.With.Type != "left" {
		return nil, fmt.Errorf("join type %q is not supported; only `left` is", step.With.Type)
	}
	if step.With.On == "" {
		return nil, fmt.Errorf("join needs `on:` to name the key")
	}

	left, err := r.collection(step.With.Left)
	if err != nil {
		return nil, fmt.Errorf("join left: %w", err)
	}
	right, err := r.collection(step.With.Right)
	if err != nil {
		return nil, fmt.Errorf("join right: %w", err)
	}

	index := make(map[string]map[string]any, len(right))
	for _, record := range right {
		if key, ok := record[step.With.On]; ok {
			index[renderScalar(key)] = record
		}
	}

	joined := make([]map[string]any, 0, len(left))
	for _, record := range left {
		merged := cloneRecord(record)
		if key, ok := record[step.With.On]; ok {
			if match, found := index[renderScalar(key)]; found {
				for _, field := range step.With.Carry {
					if value, present := match[field]; present {
						merged[field] = value
					}
				}
			}
		}
		joined = append(joined, merged)
	}
	return joined, nil
}

// derive adds a computed field by the first matching rule, then applies any
// follow-up edits.
//
// Rules are ordered and the FIRST match wins, so a file reads top to bottom.
// A rule with no `when:` is the default and must come last.
func (r *stepRunner) derive(step Step, rc *runContext) (any, error) {
	if step.With.Field == "" {
		return nil, fmt.Errorf("derive needs `field:` to name what it computes")
	}
	records, err := r.currentCollection(step, rc)
	if err != nil {
		return nil, err
	}

	out := make([]map[string]any, 0, len(records))
	for _, record := range records {
		derived := cloneRecord(record)

		value, err := r.firstMatchingRule(step.With.Rules, derived, rc)
		if err != nil {
			return nil, err
		}
		derived[step.With.Field] = value

		if err := r.applyThen(step.With.Then, derived, rc); err != nil {
			return nil, err
		}
		out = append(out, derived)
	}
	return out, nil
}

// firstMatchingRule walks the rules in order and returns the first `value:`
// whose `when:` holds. A rule without `when:` always matches.
func (r *stepRunner) firstMatchingRule(rules []map[string]any, record map[string]any, rc *runContext) (any, error) {
	for _, rule := range rules {
		when, hasWhen := rule["when"].(string)
		if !hasWhen {
			return rule["value"], nil
		}
		matched, err := r.predicate(when, record, rc)
		if err != nil {
			return nil, err
		}
		if matched {
			return rule["value"], nil
		}
	}
	return nil, fmt.Errorf("no rule matched and none is a default (a rule with no `when:`)")
}

// applyThen runs the follow-up edits a derive declares. `clear:` removes
// fields, which is how a bad coordinate is prevented from being used as
// though it were good.
func (r *stepRunner) applyThen(then []map[string]any, record map[string]any, rc *runContext) error {
	for _, edit := range then {
		if when, ok := edit["when"].(string); ok {
			matched, err := r.predicate(when, record, rc)
			if err != nil {
				return err
			}
			if !matched {
				continue
			}
		}
		for _, field := range toStrings(edit["clear"]) {
			delete(record, field)
		}
	}
	return nil
}

// dedupe collapses repeats, keeping the first occurrence.
func (r *stepRunner) dedupe(step Step, rc *runContext) (any, error) {
	if step.With.Key == "" {
		return nil, fmt.Errorf("dedupe needs `key:`")
	}
	records, err := r.currentCollection(step, rc)
	if err != nil {
		return nil, err
	}

	// A record with no key has no identity, and renderScalar turns every one
	// of them into the SAME empty string -- so they would all collapse into a
	// single survivor, silently. Keeping them all and dropping them all are
	// both guesses about data the upstream did not give us, so neither is
	// made: the run stops and says how many and on which key.
	// Presence is not enough: an explicit null is PRESENT, and renders to the
	// same empty string as an absent key -- so every record carrying
	// "marketId": null would still collapse into one survivor. Blank counts
	// too, for the same reason.
	missing := 0
	for _, record := range records {
		value, present := record[step.With.Key]
		if !present || value == nil || strings.TrimSpace(renderScalar(value)) == "" {
			missing++
		}
	}
	if missing > 0 {
		return nil, fmt.Errorf("dedupe on %q: %d of %d records carry no usable %q (absent, null or "+
			"blank), and records with no identity cannot be deduplicated -- they would all "+
			"collapse into one", step.With.Key, missing, len(records), step.With.Key)
	}

	seen := make(map[string]bool, len(records))
	out := make([]map[string]any, 0, len(records))
	for _, record := range records {
		key := renderScalar(record[step.With.Key])
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, record)
	}
	return out, nil
}

// currentCollection gives a transforming step its input: whatever the
// previous step produced, unless the file names one explicitly.
func (r *stepRunner) currentCollection(step Step, rc *runContext) ([]map[string]any, error) {
	if step.With.Left != "" {
		return r.collection(step.With.Left)
	}
	previous := r.previousOutput()
	if previous == nil {
		return nil, fmt.Errorf("%s has no input: no step before it produced one", step.Uses)
	}
	return asRecords(previous)
}

// previousOutput is what the step immediately before this one produced.
func (r *stepRunner) previousOutput() any {
	return r.lastOutput
}

// collection resolves a `${...}` reference to a list of records.
func (r *stepRunner) collection(reference string) ([]map[string]any, error) {
	if reference == "" {
		return nil, fmt.Errorf("no collection named")
	}
	resolved, err := r.rc.lookup(strings.Trim(strings.TrimPrefix(strings.TrimSpace(reference), "${"), "}"))
	if err != nil {
		return nil, err
	}
	return asRecords(resolved)
}

// condition evaluates a step's `when:`.
//
// `${inputs.states} = []` is interpolated first, then evaluated as JSONata, so
// a file can test something outside the record it is processing.
func (r *stepRunner) condition(when string) (bool, error) {
	expr, err := interpolatePredicate(r.rc, when)
	if err != nil {
		return false, err
	}
	return r.cache.truthy(expr, map[string]any{})
}

// predicate evaluates a rule's `when:` against one record.
//
// Any ${...} is substituted as a LITERAL -- see interpolatePredicate. Raw
// substitution turns `${inputs.withoutGeometry} = 'skip'` into a comparison
// against a record field that does not exist, so the rule silently never
// fires.
func (r *stepRunner) predicate(when string, record map[string]any, rc *runContext) (bool, error) {
	expr, err := interpolatePredicate(rc, when)
	if err != nil {
		return false, err
	}
	return r.cache.truthy(expr, record)
}

// elseValue supplies a skipped step's output.
func (r *stepRunner) elseValue(step Step) (any, error) {
	if step.Else.Const == "" {
		return nil, nil
	}
	resolved, err := r.rc.lookup(strings.Trim(strings.TrimPrefix(strings.TrimSpace(step.Else.Const), "${"), "}"))
	if err != nil {
		return nil, fmt.Errorf("else.const: %w", err)
	}
	// A step feeding a later forEach must produce records. A bare string here
	// would otherwise be looped over as characters, or fail much further on
	// with an error that names neither this step nor its else branch.
	if text, isText := resolved.(string); isText && text != "" {
		return nil, fmt.Errorf("else.const %s resolved to the text %q, but a step's output "+
			"must be a list of records; this file has no primitive to turn text into one",
			step.Else.Const, text)
	}
	return resolved, nil
}

// outcomeFor finds the onError entry matching this failure.
//
// The two classes a file distinguishes are the 27-of-36-states lesson written
// as data: an upstream saying "no rows" is a coverage fact, a transport
// failure is an outage, and collapsing them loses the difference.
func (r *stepRunner) outcomeFor(step Step, err error) (StepOutcome, bool) {
	class := classifyTransport
	switch {
	case errors.Is(err, ErrNoUpstreamData):
		class = classifyEmpty
	case errors.Is(err, ErrReauthRequired):
		class = classifyReauth
	}
	outcome, ok := step.OnError[class]
	return outcome, ok
}

// withReauth runs one HTTP call and, when the upstream rejected the credential
// with a status the file lists in token.reexchangeOn, obtains a fresh one and
// retries ONCE. A second rejection -- or a run that has spent its re-exchanges
// -- falls through to the step's onError like any other failure.
//
// rc is refreshed in place and so is the runner's own context, so the next
// iteration and the next step carry the new token rather than the dead one.
func (r *stepRunner) withReauth(ctx context.Context, rc *runContext, call func(*runContext) (any, error)) (any, error) {
	out, err := call(rc)
	var rejected *ReauthError
	if err == nil || r.auth == nil || !errors.As(err, &rejected) {
		return out, err
	}
	if !statusListed(r.spec.Upstream.Auth.Token.ReexchangeOn, rejected.Status) {
		return out, err
	}
	if r.reauths >= maxReauthsPerRun {
		r.log.WarnContext(ctx, "pipeline: reauth budget spent; not re-exchanging",
			"reauths", r.reauths, "status", rejected.Status)
		return out, err
	}
	r.reauths++
	r.counters["reauth"]++
	r.log.InfoContext(ctx, "pipeline: credential rejected; re-exchanging",
		"status", rejected.Status, "attempt", r.reauths)

	cred, prepErr := r.auth.Prepare(ctx, r.client, r.rc)
	if prepErr != nil {
		return nil, fmt.Errorf("re-exchanging credentials: %w", prepErr)
	}
	r.client.WithCredential(cred)
	r.rc.token = cred.Value
	rc.token = cred.Value
	return call(rc)
}

// statusListed reports whether status is one the file says means "the token
// died".
func statusListed(statuses []int, status int) bool {
	for _, s := range statuses {
		if s == status {
			return true
		}
	}
	return false
}

// record increments the counter a file named, and says what happened.
func (r *stepRunner) record(outcome StepOutcome, step Step, item any, cause error) {
	if outcome.Record == "" {
		return
	}
	r.counters[outcome.Record]++

	fields := []any{"step", step.ID, "counter", outcome.Record}
	if key, ok := item.(map[string]any); ok {
		if code, present := key["code"]; present {
			fields = append(fields, "item", renderScalar(code))
		}
	}
	if cause != nil {
		// The cause is already status-only; the client never puts a body or a
		// URL into an error, so this cannot leak the token.
		fields = append(fields, "cause", cause.Error())
	}
	r.log.Debug("pipeline: step outcome recorded", fields...)
}

// producedSummary is what a step's output looks like to its log line.
type producedSummary struct {
	count   int
	rawType string // set only when the output is not a list of records
}

func recordsForLog(output any) producedSummary {
	records, err := asRecords(output)
	if err != nil {
		return producedSummary{rawType: fmt.Sprintf("%T", output)}
	}
	return producedSummary{count: len(records)}
}

// itemLabel names a forEach item for a log line: its code when it has one.
func itemLabel(item any) string {
	if record, ok := item.(map[string]any); ok {
		if code, present := record["code"]; present {
			return renderScalar(code)
		}
	}
	return ""
}

// asRecords normalises a decoded JSON value into a list of objects.
func asRecords(value any) ([]map[string]any, error) {
	switch typed := value.(type) {
	case nil:
		return nil, nil
	case []map[string]any:
		return typed, nil
	case []any:
		out := make([]map[string]any, 0, len(typed))
		for i, item := range typed {
			record, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("item %d is %T, not an object", i, item)
			}
			out = append(out, record)
		}
		return out, nil
	case map[string]any:
		return []map[string]any{typed}, nil
	default:
		return nil, fmt.Errorf("expected a list of objects, got %T", value)
	}
}

func isEmpty(value any) bool {
	records, err := asRecords(value)
	return err == nil && len(records) == 0
}

func cloneRecord(record map[string]any) map[string]any {
	out := make(map[string]any, len(record))
	for key, value := range record {
		out[key] = value
	}
	return out
}

func toStrings(value any) []string {
	switch typed := value.(type) {
	case []string:
		return typed
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			out = append(out, renderScalar(item))
		}
		return out
	case string:
		return []string{typed}
	default:
		return nil
	}
}

// sortRecords is used by the catalog builder's `order:` block.
func sortRecords(records []map[string]any, by, direction string) {
	descending := strings.EqualFold(direction, "desc")
	sort.SliceStable(records, func(i, j int) bool {
		left, right := renderScalar(records[i][by]), renderScalar(records[j][by])
		// Numeric keys must order numerically: as text, market 9 sorts after
		// market 10, and the chunk boundaries move with it.
		li, lok := numeric(records[i][by])
		ri, rok := numeric(records[j][by])
		if lok && rok {
			if descending {
				return li > ri
			}
			return li < ri
		}
		if descending {
			return left > right
		}
		return left < right
	})
}

// sortRecordsBy orders records by each key in turn, ascending: numbers
// numerically, anything else as text compared case-insensitively, so
// "chandrapur" sits beside "Chandrapur" rather than after "Zirakpur". It is
// the catalog builder's `order.renderBy`, the order a reader sees.
func sortRecordsBy(records []map[string]any, keys []string) {
	sort.SliceStable(records, func(i, j int) bool {
		for _, key := range keys {
			if li, lok := numeric(records[i][key]); lok {
				if ri, rok := numeric(records[j][key]); rok {
					if li != ri {
						return li < ri
					}
					continue
				}
			}
			left := strings.ToLower(strings.TrimSpace(renderScalar(records[i][key])))
			right := strings.ToLower(strings.TrimSpace(renderScalar(records[j][key])))
			if left != right {
				return left < right
			}
		}
		return false
	})
}

func numeric(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	default:
		return 0, false
	}
}

// httpPost calls the upstream via POST through the step's mapping.
//
// Situation: the upstream endpoint requires POST (search APIs, submission
//
//	endpoints, or APIs that put filters in the request body).
//
// Scenario:  The mapping's request half builds a JSON body; the response
//
//	half shapes the returned data into pipeline records.
//
// YAML usage:
//
//   - id: results
//     uses: http.post
//     with:
//     path: /v1/search
//     mapping: mappings/search.yaml
//     local: { token: "${auth.token}", query: "${inputs.searchQuery}" }
//     out: results
func (r *stepRunner) httpPost(ctx context.Context, step Step, rc *runContext) (any, error) {
	if r.client == nil {
		return nil, errNoUpstream(step)
	}
	if step.With.Path == "" || step.With.Mapping == "" {
		return nil, fmt.Errorf("http.post needs both `path:` and `mapping:`")
	}
	path, err := rc.interpolate(step.With.Path)
	if err != nil {
		return nil, err
	}
	ref, err := r.mappingRef(step.With.Mapping)
	if err != nil {
		return nil, err
	}

	local := map[string]any{}
	for k, v := range step.With.Local {
		resolved, err := rc.interpolate(v)
		if err != nil {
			return nil, fmt.Errorf("http.post local %q: %w", k, err)
		}
		local[k] = resolved
	}

	raw, err := r.client.Post(ctx, r.mapper, ref, path, local)
	if err != nil {
		return nil, err
	}
	return decodeMapped(raw)
}

// filter keeps records in the current collection that match a JSONata predicate.
//
// Situation: mid-pipeline cleanup before a join or dedupe — e.g. remove rows
//
//	with a blank key, or rows the upstream marks as inactive.
//
// Scenario:  Evaluate with.when per record; keep only the truthy ones.
//
// YAML usage:
//
//   - id: active
//     uses: filter
//     with:
//     when: "$exists(itemId) and itemId != ”"
//     out: active
func (r *stepRunner) filter(step Step, rc *runContext) (any, error) {
	if step.With.When == "" {
		return nil, fmt.Errorf("filter needs `with.when:` — a JSONata predicate to keep records by")
	}
	records, err := r.currentCollection(step, rc)
	if err != nil {
		return nil, err
	}
	kept := make([]map[string]any, 0, len(records))
	for _, rec := range records {
		// Through predicate, not straight to truthy: a ${...} must be
		// substituted as a LITERAL. Raw, "${inputs.mode} = 'skip'" compares a
		// record field that does not exist and the filter silently keeps
		// everything -- the same bug derive and exclude already had.
		ok, err := r.predicate(step.With.When, rec, rc)
		if err != nil {
			return nil, fmt.Errorf("filter when %q: %w", step.With.When, err)
		}
		if ok {
			kept = append(kept, rec)
		}
	}
	return kept, nil
}

// transform applies a JSONata mapping to records in the current collection
// without making an HTTP call.
//
// Situation: enrich or reshape pipeline records inline — add a computed field,
//
//	reformat keys, or project a subset — before a join or catalog step.
//
// Scenario:  Run the mapping's response half over all records; the request
//
//	half is skipped (no upstream call needed).
//
// YAML usage:
//
//   - id: enriched
//     uses: transform
//     with:
//     mapping: mappings/enrich.yaml
//     local: { networkId: "${inputs.networkId}" }
//     out: enriched
func (r *stepRunner) transform(ctx context.Context, step Step, rc *runContext) (any, error) {
	if step.With.Mapping == "" {
		return nil, fmt.Errorf("transform needs `with.mapping:`")
	}
	records, err := r.currentCollection(step, rc)
	if err != nil {
		return nil, err
	}
	ref, err := r.mappingRef(step.With.Mapping)
	if err != nil {
		return nil, err
	}

	local := map[string]any{}
	for k, v := range step.With.Local {
		resolved, err := rc.interpolate(v)
		if err != nil {
			return nil, fmt.Errorf("transform local %q: %w", k, err)
		}
		local[k] = resolved
	}

	out, err := r.mapper.Transform(ctx, ref, definition.DirectionResponse, map[string]any{
		"response": records,
		"_local":   local,
	})
	if err != nil {
		return nil, fmt.Errorf("transform %s: %w", step.With.Mapping, err)
	}
	return decodeMapped(out)
}

// decodeMapped turns a mapping's JSON output into the step's collection.
//
// The mapper answers with JSON bytes, which asRecords does not read -- handing
// them over undecoded failed every http.post and transform step with
// "expected a list of objects, got []uint8". httpGet decodes the same way.
func decodeMapped(raw []byte) ([]map[string]any, error) {
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("the mapped response did not decode: %w", err)
	}
	return asRecords(decoded)
}

// constRecords is the const primitive: the records the file declares, handed
// on as the step's output.
//
// Each is a copy. Catalog rendering writes resourceId into every record,
// and handing out the spec's own maps would write that back into the parsed
// file -- harmless for one run, wrong the moment a Spec is reused.
//
// An empty list is refused rather than passed on: an empty collection walks
// cleanly through every later step and produces a run that reads as "this
// provider has nothing to publish" and exits zero.
func (r *stepRunner) constRecords(step Step) (any, error) {
	if len(step.With.Records) == 0 {
		return nil, fmt.Errorf("const needs at least one entry in `with.records:`")
	}
	out := make([]map[string]any, 0, len(step.With.Records))
	for _, record := range step.With.Records {
		out = append(out, cloneRecord(record))
	}
	return out, nil
}

// errNoUpstream is what an HTTP step reports in a pipeline that declares no
// upstream. The schema refuses that file at load; this is the backstop that
// keeps a nil client from becoming a panic if one gets through anyway.
func errNoUpstream(step Step) error {
	return fmt.Errorf("%s needs an upstream, and this pipeline declares no `upstream:` block", step.Uses)
}

// ---------------------------------------------------------------------------
// publish: which built catalogs go out, in what order, under what safety rule,
// and what counts as success.
//
// This does NOT make the HTTP call. That is a Publisher's -- in production the
// crawler's sink (catalogcrawler/internal/sink), the one piece of code that
// posts to the provider adapter's /publish, for crawled catalogs and pipelines
// alike. Keeping the call out of here is what lets this package stay free of
// the crawler while the crawler owns the network.
// ---------------------------------------------------------------------------

// Per-catalog outcomes.
const (
	StatusPublished      = "published"
	StatusRejected       = "rejected"
	StatusTransportError = "transport-error"
)

// Outcome is what happened to one catalog.
type Outcome struct {
	Group     string
	CatalogID string
	Status    string
	Reason    string
}

// Result is one publish step.
type Result struct {
	Outcomes   []Outcome
	RetiredOld *Outcome

	// RetiredSkipped says why a declared retirement did NOT happen. Without
	// it, a skipped retirement and a pipeline that never asked for one look
	// identical in the report.
	RetiredSkipped string
}

// HasFailures reports whether anything did not reach the index intact, so a
// caller can exit non-zero. A PARTIAL counts: a Publisher reports it as
// StatusRejected.
func (r Result) HasFailures() bool {
	for _, outcome := range r.Outcomes {
		if outcome.Status != StatusPublished {
			return true
		}
	}
	return r.RetiredOld != nil && r.RetiredOld.Status != StatusPublished
}

// Publisher posts one catalog/publish body to the provider adapter whose
// base address is baseURL, and judges the answer: ACCEPTED is
// StatusPublished, anything else -- PARTIAL included -- is StatusRejected,
// and a failure to reach it is StatusTransportError. A transport failure is
// an Outcome, not an error, so one bad catalog never hides the others.
type Publisher interface {
	Publish(ctx context.Context, baseURL string, body []byte) Outcome

	// Retire deactivates a superseded catalog. The Publisher builds the
	// body -- with the same builder as every publish -- so a retirement
	// carries the same identity, and every field the file's retireOld states.
	Retire(ctx context.Context, baseURL string, retirement Retirement) Outcome
}

// Retirement is a resolved retireOld block: what to deactivate, and the
// directive it goes out under.
type Retirement struct {
	CatalogID      string
	DescriptorName string
	CatalogType    string   // REGULAR unless the file says otherwise
	UpdateMode     string   // MERGE or FULL
	VisibleTo      []string // the networks the retired catalog was visible to
	SchemaTypes    []string // the retired catalog's JSON-LD schema contexts
}

// publishAddressHint is the fallback when a caller has not said how THIS
// pipeline's address is supplied. Generic on purpose: naming one pipeline's
// variable here once told every other pipeline to set it.
const publishAddressHint = "set inputs.publishUrl"

// publishAddressHintFor names the ways an operator can supply a pipeline's
// publish address -- its own flag and env, as the file declares them -- so an
// empty one says what to set rather than only that something is missing.
func publishAddressHintFor(input Input) string {
	var ways []string
	if input.Flag != "" {
		ways = append(ways, "flag --"+input.Flag)
	}
	if input.Env != "" {
		ways = append(ways, input.Env)
	}
	if len(ways) == 0 {
		return publishAddressHint
	}
	return publishAddressHint + " (" + strings.Join(ways, " or ") + ")"
}

// refuseWhenShape is the one form of refusal this step understands:
//
//	collection.<counter> > <N>
//
// <counter> is a name the pipeline's own steps record into, so a second
// pipeline refuses on its own terms without editing Go, and N is a
// non-negative integer: `> 0` refuses on any failure, `> 3` tolerates three.
// Anything else is REJECTED rather than ignored -- a safety rule that is
// quietly dropped is worse than one never declared -- and a file needing a
// richer rule needs a real expression evaluator here first.
var refuseWhenShape = regexp.MustCompile(`^collection\.([A-Za-z][A-Za-z0-9_]*)\s*>\s*([0-9]+)$`)

// refuseWhenRule returns the counter a refuseWhen rule names and the count it
// tolerates.
func refuseWhenRule(rule string) (string, int, bool) {
	match := refuseWhenShape.FindStringSubmatch(strings.TrimSpace(rule))
	if match == nil {
		return "", 0, false
	}
	threshold, err := strconv.Atoi(match[2])
	if err != nil {
		return "", 0, false
	}
	return match[1], threshold, true
}

// PublishCatalogs posts every catalog file in catalogDir through
// publisher, unless the declared safety rule forbids it.
//
// collected is every counter the run recorded; publish.refuseWhen names the
// one it judges. A part that failed to collect is not a part with nothing in
// it, so publishing past the file's tolerance would replace a whole group's
// catalog with a partial one, or with nothing.
//
// Sequential and per-catalog on purpose: one failed catalog is one
// retryable catalog, and must not discard the outcomes of the others.
func PublishCatalogs(ctx context.Context, spec Publish, resolved map[string]string,
	catalogDir, filenamePrefix string, collected map[string]int, publisher Publisher) (Result, error) {
	var result Result

	if publisher == nil {
		return result, fmt.Errorf("no publisher: this run was asked to publish but given nothing to publish with")
	}

	if rule := strings.TrimSpace(spec.RefuseWhen); rule != "" {
		counter, threshold, ok := refuseWhenRule(rule)
		if !ok {
			return result, fmt.Errorf(
				"publish.refuseWhen is %q; this step understands only `collection.<counter> > <N>` "+
					"and will not guess at another rule", rule)
		}
		if failed := collected[counter]; failed > threshold {
			return result, fmt.Errorf(
				"refusing to publish: %d of the collection failed (%s), above the %d this pipeline "+
					"tolerates, and a partial collection will not be published as though it were whole "+
					"(publish.refuseWhen: %s)", failed, counter, threshold, rule)
		}
	}

	// The YAML's publish.url is ${inputs.publishUrl}/publish, but a Publisher
	// is given the BASE address and appends /publish itself. Passing the
	// rendered url would post to /publish/publish, which fails as a 404 far
	// from here and reads like an unreachable adapter rather than a bug.
	if err := checkPublishURL(spec); err != nil {
		return result, err
	}

	hint := spec.AddressHint
	if hint == "" {
		hint = publishAddressHint
	}
	publishURL := strings.TrimSpace(resolved["publishUrl"])
	if publishURL == "" {
		return result, fmt.Errorf("no publish address: %s", hint)
	}

	retire, err := retirement(spec.RetireOld, resolved)
	if err != nil {
		return result, err
	}

	files, err := catalogFiles(catalogDir, filenamePrefix)
	if err != nil {
		return result, err
	}
	// An empty directory must not look like a successful run: silently
	// publishing nothing is indistinguishable from publishing everything.
	if len(files) == 0 && retire == nil {
		return result, fmt.Errorf("no catalog files in %s", catalogDir)
	}

	for _, file := range files {
		result.Outcomes = append(result.Outcomes, publishFile(ctx, publisher, publishURL, file))
	}
	// The retirement goes out ONLY when everything that supersedes the old
	// catalog is actually on the network.
	//
	// Retiring is not a tidy-up, it is a deletion: deactivating the old
	// catalog is how its resources leave. Sending it after a run whose
	// replacements were rejected removes the old data and puts nothing in its
	// place, leaving the network with neither -- and the next tick repeats it.
	if retire != nil {
		if published, why := allPublished(result.Outcomes); !published {
			result.RetiredSkipped = why
		} else {
			outcome := publisher.Retire(ctx, publishURL, *retire)
			if outcome.CatalogID == "" {
				outcome.CatalogID = retire.CatalogID
			}
			result.RetiredOld = &outcome
		}
	}
	return result, nil
}

// catalogFile pairs a built catalog with the group it belongs to.
//
// slug and group differ for a split group: <prefix>-TN-2.json has slug
// TN-2 and group TN.
type catalogFile struct {
	slug  string
	group string
	path  string
}

// chunkSuffix matches the -N a split group's catalog carries.
var chunkSuffix = regexp.MustCompile(`-[0-9]+$`)

// catalogFiles lists <prefix>-<slug>.json in dir, sorted, so a run reads the
// same way twice. The match is the one WriteCatalogs and
// RemoveStaleCatalogs make -- three places, one naming contract.
func catalogFiles(dir, prefix string) ([]catalogFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read catalog directory: %w", err)
	}
	namePrefix := prefix + "-"
	var files []catalogFile
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, namePrefix) || !strings.HasSuffix(name, ".json") {
			continue
		}
		slug := strings.TrimSuffix(strings.TrimPrefix(name, namePrefix), ".json")
		files = append(files, catalogFile{
			slug: slug, group: chunkSuffix.ReplaceAllString(slug, ""), path: filepath.Join(dir, name),
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].slug < files[j].slug })
	return files, nil
}

// publishFile reads one built catalog and posts it VERBATIM: the file is
// what a human reviewed, so re-encoding it would publish something nobody
// read.
func publishFile(ctx context.Context, publisher Publisher, publishURL string, file catalogFile) Outcome {
	body, err := os.ReadFile(file.path)
	if err != nil {
		return Outcome{Group: file.group, Status: StatusTransportError,
			Reason: fmt.Sprintf("read %s: %v", file.path, err)}
	}
	outcome := publisher.Publish(ctx, publishURL, body)
	outcome.Group = file.group
	if outcome.CatalogID == "" {
		outcome.CatalogID = catalogIDOf(body)
	}
	return outcome
}

// catalogIDOf reads the catalog's own id out of a publish body, so a
// reported id is the one actually sent rather than one rebuilt from a name.
func catalogIDOf(body []byte) string {
	var envelope struct {
		Message struct {
			Catalogs []struct {
				ID string `json:"id"`
			} `json:"catalogs"`
		} `json:"message"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && len(envelope.Message.Catalogs) > 0 {
		return envelope.Message.Catalogs[0].ID
	}
	return ""
}

// expectedPublishURL is the only publish.url this step can honour, for the
// reason given where the address is resolved: the base goes to
// the Publisher, which appends the path itself.
const expectedPublishURL = "${inputs.publishUrl}/publish"

// checkPublishURL refuses a publish.url this step would ignore.
//
// The address actually used comes from inputs.publishUrl, not from this
// field. Without this check an operator could repoint publish.url at another
// host, watch the edit take no effect, and have the catalog posted to
// CATALOG_PUBLISH_URL anyway -- a rule the file states and nothing enforces,
// which is also why refuseWhen is parsed strictly above.
func checkPublishURL(spec Publish) error {
	if url := strings.TrimSpace(spec.URL); url != expectedPublishURL {
		return fmt.Errorf(
			"publish.url is %q, but this step publishes to inputs.publishUrl and would ignore it; "+
				"only %q is honoured", url, expectedPublishURL)
	}
	return nil
}

// retirement resolves the declared retireOld block: nil when there is none
// or it is switched off, the block itself when it is on.
//
// A declared retireOld that never reached the publish step would be a rule
// the file states and nothing performs.
//
// Enabled is an `${inputs.*}` reference. One naming an input the file does not
// declare is refused rather than read as false: treating an unresolvable
// enable flag as "off" would silently skip the retirement, which is the
// outcome an operator who wrote the block was trying to avoid.
func retirement(spec RetireOld, resolved map[string]string) (*Retirement, error) {
	enabled := strings.TrimSpace(spec.Enabled)
	if enabled == "" {
		return nil, nil // no retireOld block
	}

	value, ok := resolved[strings.TrimSuffix(strings.TrimPrefix(enabled, "${inputs."), "}")]
	if !ok {
		return nil, fmt.Errorf(
			"publish.retireOld.enabled is %q, but no such input is declared, so the retirement "+
				"cannot be turned on or off; declare the input or remove the block", enabled)
	}
	if !strings.EqualFold(value, "true") {
		return nil, nil
	}
	if strings.TrimSpace(spec.CatalogID) == "" {
		return nil, fmt.Errorf("publish.retireOld is enabled but names no catalogId to retire")
	}
	if spec.IsActive != nil && *spec.IsActive {
		return nil, fmt.Errorf("publish.retireOld.isActive is true, but a retirement deactivates the catalog; " +
			"remove it or set it to false")
	}

	out := Retirement{
		CatalogID:      strings.TrimSpace(spec.CatalogID),
		DescriptorName: spec.DescriptorName,
		CatalogType:    orDefault(spec.CatalogType, "REGULAR"),
		UpdateMode:     strings.ToUpper(orDefault(spec.UpdateMode, "MERGE")),
	}
	if out.UpdateMode != "MERGE" && out.UpdateMode != "FULL" {
		return nil, fmt.Errorf("publish.retireOld.updateMode is %q; use MERGE or FULL", spec.UpdateMode)
	}
	var err error
	if out.VisibleTo, err = resolveInputList("visibleTo", spec.VisibleTo, resolved); err != nil {
		return nil, err
	}
	if out.SchemaTypes, err = resolveInputList("schemaTypes", spec.SchemaTypes, resolved); err != nil {
		return nil, err
	}
	return &out, nil
}

// resolveInputList resolves each entry: a whole-entry ${inputs.name} becomes
// that input's value, anything else is taken as written. An entry naming an
// input that is not declared, or that resolves to empty, is refused -- a
// visibility list with a hole in it scopes the deactivation to nobody.
func resolveInputList(field string, entries []string, resolved map[string]string) ([]string, error) {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if name, isRef := strings.CutPrefix(entry, "${inputs."); isRef && strings.HasSuffix(name, "}") {
			name = strings.TrimSuffix(name, "}")
			value, declared := resolved[name]
			if !declared || strings.TrimSpace(value) == "" {
				return nil, fmt.Errorf("publish.retireOld.%s names %s, which is not declared or resolved to empty",
					field, entry)
			}
			entry = value
		}
		if entry != "" {
			out = append(out, entry)
		}
	}
	return out, nil
}

func orDefault(value, fallback string) string {
	if v := strings.TrimSpace(value); v != "" {
		return v
	}
	return fallback
}

// allPublished reports whether every catalog this run built actually
// reached the network, and says what stopped it when one did not.
//
// A run that built NOTHING does not qualify either: an empty directory plus a
// retirement would deactivate the old catalog and replace it with nothing at
// all, which is the same harm by a quieter route.
func allPublished(outcomes []Outcome) (bool, string) {
	if len(outcomes) == 0 {
		return false, "no catalogs were built, so there is nothing to supersede the old one"
	}
	for _, outcome := range outcomes {
		if outcome.Status != StatusPublished {
			return false, fmt.Sprintf("%s is %s, so the old catalog still has to serve its resources",
				outcome.CatalogID, outcome.Status)
		}
	}
	return true, ""
}
