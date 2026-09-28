package pipeline

// steps.go runs the `pipeline:` block: the ordered list of steps a file
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
	"sort"
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
	// evaluations (expr.go's lock) at the same time over that shared state --
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
