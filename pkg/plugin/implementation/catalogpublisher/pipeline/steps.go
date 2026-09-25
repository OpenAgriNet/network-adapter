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
// last one, which is the collection the catalogue is built from.
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
		output, err := r.runStep(ctx, step)
		if err != nil {
			return nil, fmt.Errorf("step %q: %w", step.ID, err)
		}

		produced, _ := asRecords(output)
		r.log.InfoContext(ctx, "pipeline step: done",
			"step", fmt.Sprintf("%d/%d", i+1, len(r.spec.Pipeline)),
			"id", step.ID, "records", len(produced),
			"took", time.Since(began).Round(time.Millisecond).String())
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

// runStep applies one step's control flow -- the conditional, the loop -- and
// dispatches the primitive underneath.
func (r *stepRunner) runStep(ctx context.Context, step Step) (any, error) {
	// A step's `when:` decides whether it runs at all; `else: const:` supplies
	// what its output is when it does not.
	if step.When != "" {
		run, err := r.condition(step.When)
		if err != nil {
			return nil, err
		}
		if !run {
			return r.elseValue(step)
		}
	}

	output, err := r.dispatch(ctx, step)
	if err != nil {
		return nil, err
	}

	if step.FailWhenEmpty != "" && isEmpty(output) {
		// Not a warning. An empty collection walks cleanly through every
		// later step and produces a well-formed, zero-error, empty result --
		// a run that reads as "there is nothing to publish" and exits zero.
		return nil, fmt.Errorf("%s", step.FailWhenEmpty)
	}
	return output, nil
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

		output, err := r.primitive(ctx, step, iteration)
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
		return r.httpGet(ctx, step, rc)
	case usesHTTPPost:
		return r.httpPost(ctx, step, rc)
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

	raw, err := r.client.Get(ctx, r.mapper, r.mappingRef(step.With.Mapping), path, local)
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
// resolves. A file writes `mappings/master-states.yaml`; the mappings are
// served from the root of that directory.
func (r *stepRunner) mappingRef(mapping string) string {
	// The base is a URL, so this is deliberately string concatenation and not
	// filepath.Join: Join collapses the "//" in "http://host" to "http:/",
	// producing an address nothing can fetch. Two call sites used Join and
	// were broken for every mapping, not merely prefixed ones.
	//
	// The "mappings/" prefix is stripped because a file writes its mappings
	// relative to itself, while they are SERVED from the root of that
	// directory. Both spellings therefore resolve to the same ref, so a
	// provider cannot be wrong about which one to use.
	return strings.TrimRight(r.mappingBase, "/") + "/" + strings.TrimPrefix(mapping, "mappings/")
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
	missing := 0
	for _, record := range records {
		if _, present := record[step.With.Key]; !present {
			missing++
		}
	}
	if missing > 0 {
		return nil, fmt.Errorf("dedupe on %q: %d of %d records carry no %q, and records with no "+
			"identity cannot be deduplicated -- they would all collapse into one",
			step.With.Key, missing, len(records), step.With.Key)
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
	class := "transportError"
	if errors.Is(err, ErrNoUpstreamData) {
		class = "emptyResult"
	}
	outcome, ok := step.OnError[class]
	return outcome, ok
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

// sortRecords is used by the catalogue builder's `order:` block.
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
	ref := r.mappingRef(step.With.Mapping)

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
	ref := r.mappingRef(step.With.Mapping)

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
// Each is a copy. Catalogue rendering writes resourceId into every record,
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
