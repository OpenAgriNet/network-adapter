package pipeline

// interpreter_test.go pins the interpreter against the control flow a pipeline file
// can declare. Every case here is a rule the YAML states and the runner has to
// honour, because the whole point of the interpreter is that the file is the
// program: a rule the runner quietly ignores is worse than one never written,
// since the file reads as though it were in force.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

// passthroughMapper returns the request's own local map as the query, and the
// upstream's body unchanged as the response. It lets a step test exercise the
// runner without a real JSONata mapping.
type passthroughMapper struct{}

func (passthroughMapper) Verify(context.Context, string, any) error { return nil }

func (passthroughMapper) Transform(_ context.Context, _ string, d definition.Direction, in any) ([]byte, error) {
	input, _ := in.(map[string]any)
	if d == definition.DirectionRequest {
		local, _ := input["_local"].(map[string]any)
		return json.Marshal(local)
	}
	return json.Marshal(input["response"])
}

// testRunner builds a runner over a fake upstream serving body.
func testRunner(t *testing.T, spec Spec, body string) (*stepRunner, *httptest.Server) {
	t.Helper()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(upstream.Close)

	cache, err := newExprCache()
	if err != nil {
		t.Fatalf("newExprCache: %v", err)
	}
	client := NewClient(upstream.URL).WithErrorRules(testErrorRules())
	client.http = upstream.Client()

	return &stepRunner{
		spec:     spec,
		rc:       newRunContext(map[string]string{}, "tok-fake"),
		cache:    cache,
		client:   client,
		mapper:   passthroughMapper{},
		log:      slog.New(slog.DiscardHandler),
		counters: map[string]int{},
	}, upstream
}

func TestRunStepsExecutesInOrderAndNamesOutputs(t *testing.T) {
	spec := Spec{Pipeline: []Step{
		{ID: "fetch", Uses: usesHTTPGet, Out: "rows",
			With: With{Path: "/things", Mapping: "mappings/things.yaml"}},
		{ID: "dedupe", Uses: usesDedupe, Out: "collection", With: With{Key: "id"}},
	}}
	runner, _ := testRunner(t, spec, `[{"id":1},{"id":1},{"id":2}]`)

	records, err := runner.runSteps(context.Background())
	if err != nil {
		t.Fatalf("runSteps: %v", err)
	}
	if len(records) != 2 {
		t.Errorf("got %d records after dedupe, want 2", len(records))
	}
	// The intermediate output must still be reachable by name -- later steps
	// and the catalog block address it that way.
	if _, ok := runner.rc.outputs["rows"]; !ok {
		t.Error("the first step's output was not recorded under its `out:` name")
	}
}

// A cancelled context must refuse to run ANY further step, including one
// with no upstream call (const, derive, dedupe...): those have no way of
// their own to notice a shutdown or a deadline, and would otherwise run to
// completion and still reach Publish with a context that already ended.
func TestRunStepsRefusesToRunOnAnAlreadyCancelledContext(t *testing.T) {
	spec := Spec{Pipeline: []Step{
		{ID: "seed", Uses: usesConst, Out: "rows", With: With{Records: []map[string]any{{"id": 1}}}},
	}}
	runner, _ := testRunner(t, spec, `[]`)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := runner.runSteps(ctx)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("runSteps error = %v, want it to wrap context.Canceled", err)
	}
	if _, ran := runner.rc.outputs["rows"]; ran {
		t.Error("a step ran after the context was already cancelled")
	}
}

// `when:` false must skip the call entirely and take `else: const:`. This is
// how the real file avoids fetching a state list it was handed.
func TestStepWhenFalseTakesElseAndDoesNotCall(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_, _ = w.Write([]byte(`[{"code":"XX"}]`))
	}))
	defer upstream.Close()

	cache, _ := newExprCache()
	client := NewClient(upstream.URL).WithErrorRules(testErrorRules())
	client.http = upstream.Client()

	spec := Spec{Pipeline: []Step{{
		ID: "states", Uses: usesHTTPGet, Out: "states",
		When: "${inputs.supplied} = 'yes'",
		Else: StepElse{Const: "${preset}"},
		With: With{Path: "/states", Mapping: "mappings/states.yaml"},
	}}}

	runner := &stepRunner{
		spec: spec, cache: cache, client: client, mapper: passthroughMapper{},
		log: slog.New(slog.DiscardHandler), counters: map[string]int{},
		rc: newRunContext(map[string]string{"supplied": "no"}, "tok"),
	}
	runner.rc.outputs["preset"] = []any{map[string]any{"code": "MH"}}

	records, err := runner.runSteps(context.Background())
	if err != nil {
		t.Fatalf("runSteps: %v", err)
	}
	if called {
		t.Error("the upstream was called even though `when:` was false")
	}
	if len(records) != 1 || records[0]["code"] != "MH" {
		t.Errorf("records = %v, want the else: const value", records)
	}
}

// failWhenEmpty is a refusal, not a warning: an empty collection walks
// cleanly through every later step and produces a well-formed empty result.
func TestStepFailWhenEmptyRefusesAnEmptyResult(t *testing.T) {
	spec := Spec{Pipeline: []Step{{
		ID: "states", Uses: usesHTTPGet, Out: "states",
		With:          With{Path: "/states", Mapping: "mappings/states.yaml"},
		FailWhenEmpty: "state list resolved to zero states",
	}}}
	runner, _ := testRunner(t, spec, `[]`)

	_, err := runner.runSteps(context.Background())
	if err == nil {
		t.Fatal("an empty result passed a step declaring failWhenEmpty")
	}
	if !strings.Contains(err.Error(), "zero states") {
		t.Errorf("error %q does not carry the file's own message", err)
	}
}

// The 27-of-36-states lesson, as the file states it: "no rows for this item"
// is a coverage fact to count and continue past; a transport failure is an
// outage. Collapsing them is what turned quiet states into reported outages.
func TestForEachClassifiesEmptyResultSeparatelyFromTransportError(t *testing.T) {
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.RawQuery, "QUIET"):
			http.Error(w, `{"success":false,"message":"No data available."}`, http.StatusBadRequest)
		case strings.Contains(r.URL.RawQuery, "BROKEN"):
			http.Error(w, `{"error":"upstream is down"}`, http.StatusInternalServerError)
		default:
			_, _ = w.Write([]byte(`[{"marketId":1}]`))
		}
	}))
	defer upstream.Close()

	cache, _ := newExprCache()
	client := NewClient(upstream.URL).WithErrorRules(testErrorRules())
	client.http = upstream.Client()

	spec := Spec{Pipeline: []Step{{
		ID: "rows", Uses: usesHTTPGet, Out: "rows",
		ForEach: "${states}", As: "state", Concurrency: 1,
		With: With{Path: "/rows", Mapping: "mappings/rows.yaml",
			Local: map[string]string{"stateCode": "${state.code}"}},
		OnError: map[string]StepOutcome{
			"emptyResult":    {Record: "emptyGroups", Continue: true},
			"transportError": {Record: "groupErrors", Continue: true},
		},
	}}}

	runner := &stepRunner{
		spec: spec, cache: cache, client: client, mapper: passthroughMapper{},
		log: slog.New(slog.DiscardHandler), counters: map[string]int{},
		rc: newRunContext(map[string]string{}, "tok"),
	}
	runner.rc.outputs["states"] = []any{
		map[string]any{"code": "GOOD"},
		map[string]any{"code": "QUIET"},
		map[string]any{"code": "BROKEN"},
	}

	records, err := runner.runSteps(context.Background())
	if err != nil {
		t.Fatalf("a quiet state or a broken one aborted the whole run: %v", err)
	}
	if len(records) != 1 {
		t.Errorf("got %d records, want 1 (only the healthy state had rows)", len(records))
	}
	if runner.counters["emptyGroups"] != 1 {
		t.Errorf("emptyGroups = %d, want 1", runner.counters["emptyGroups"])
	}
	if runner.counters["groupErrors"] != 1 {
		t.Errorf("groupErrors = %d, want 1 -- a 500 is an outage, not a quiet state",
			runner.counters["groupErrors"])
	}
}

// A cancel mid-forEach must stop the loop and surface as the context's error.
// Otherwise every remaining item fails fast into groupErrors, and a forEach
// that is the last step hands PublishCatalogs a run that reads as "too many
// failed groups" rather than as the shutdown it was.
func TestForEachStopsOnCancelInsteadOfRecordingEachRemainingItem(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		cancel() // the shutdown lands while the first item is in flight
		_, _ = w.Write([]byte(`[{"marketId":1}]`))
	}))
	defer upstream.Close()

	cache, _ := newExprCache()
	client := NewClient(upstream.URL).WithErrorRules(testErrorRules())
	client.http = upstream.Client()

	spec := Spec{Pipeline: []Step{{
		ID: "rows", Uses: usesHTTPGet, Out: "rows",
		ForEach: "${states}", As: "state", Concurrency: 1,
		With: With{Path: "/rows", Mapping: "mappings/rows.yaml",
			Local: map[string]string{"stateCode": "${state.code}"}},
		OnError: map[string]StepOutcome{
			"transportError": {Record: "groupErrors", Continue: true},
		},
	}}}
	runner := &stepRunner{
		spec: spec, cache: cache, client: client, mapper: passthroughMapper{},
		log: slog.New(slog.DiscardHandler), counters: map[string]int{},
		rc: newRunContext(map[string]string{}, "tok"),
	}
	runner.rc.outputs["states"] = []any{
		map[string]any{"code": "A"}, map[string]any{"code": "B"}, map[string]any{"code": "C"},
	}

	_, err := runner.runSteps(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runSteps error = %v, want it to wrap context.Canceled", err)
	}
	if !strings.Contains(err.Error(), `step "rows"`) || strings.Count(err.Error(), "step ") != 1 {
		t.Errorf("error %q should name the step exactly once", err)
	}
	if got := runner.counters["groupErrors"]; got != 0 {
		t.Errorf("groupErrors = %d, want 0 -- a cancel is not a failed group", got)
	}
	if got := calls.Load(); got > 1 {
		t.Errorf("upstream called %d times, want at most 1 -- items after the cancel must not run", got)
	}
}

// Without a matching onError entry a failure must abort, not be swallowed.
func TestForEachWithoutAMatchingOnErrorAborts(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"down"}`, http.StatusInternalServerError)
	}))
	defer upstream.Close()

	cache, _ := newExprCache()
	client := NewClient(upstream.URL).WithErrorRules(testErrorRules())
	client.http = upstream.Client()

	spec := Spec{Pipeline: []Step{{
		ID: "rows", Uses: usesHTTPGet, Out: "rows",
		ForEach: "${states}", As: "state",
		With:    With{Path: "/rows", Mapping: "mappings/rows.yaml"},
		OnError: map[string]StepOutcome{"emptyResult": {Record: "empty", Continue: true}},
	}}}
	runner := &stepRunner{
		spec: spec, cache: cache, client: client, mapper: passthroughMapper{},
		log: slog.New(slog.DiscardHandler), counters: map[string]int{},
		rc: newRunContext(map[string]string{}, "tok"),
	}
	runner.rc.outputs["states"] = []any{map[string]any{"code": "A"}}

	if _, err := runner.runSteps(context.Background()); err == nil {
		t.Fatal("a transport failure with no matching onError was swallowed")
	}
}

// Concurrency above 1 must be refused, not accepted and ignored. A file that
// states a parallelism which never happens is a lie the reader cannot see.
func TestForEachRefusesUnsupportedConcurrency(t *testing.T) {
	spec := Spec{Pipeline: []Step{{
		ID: "rows", Uses: usesHTTPGet, ForEach: "${states}", As: "state", Concurrency: 4,
		With: With{Path: "/rows", Mapping: "mappings/rows.yaml"},
	}}}
	runner, _ := testRunner(t, spec, `[]`)
	runner.rc.outputs["states"] = []any{map[string]any{"code": "A"}}

	_, err := runner.runSteps(context.Background())
	if err == nil {
		t.Fatal("concurrency: 4 was accepted")
	}
	if !strings.Contains(err.Error(), "concurrency") {
		t.Errorf("error %q does not name the unsupported setting", err)
	}
}

// A left row with NO match is kept: it is a real record whose extra fields are
// merely unknown. Dropping it would silently lose markets that trade today.
func TestJoinKeepsUnmatchedLeftRows(t *testing.T) {
	spec := Spec{Pipeline: []Step{{
		ID: "join", Uses: usesJoin, Out: "joined",
		With: With{Left: "${rows}", Right: "${master}", On: "marketId",
			Type: "left", Carry: []string{"latitude", "longitude"}},
	}}}
	runner, _ := testRunner(t, spec, `[]`)
	runner.rc.outputs["rows"] = []any{
		map[string]any{"marketId": float64(1), "name": "Pune"},
		map[string]any{"marketId": float64(99), "name": "Unlisted"},
	}
	runner.rc.outputs["master"] = []any{
		map[string]any{"marketId": float64(1), "latitude": 18.5, "longitude": 73.8},
	}

	records, err := runner.runSteps(context.Background())
	if err != nil {
		t.Fatalf("runSteps: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2 -- the unmatched row must be kept", len(records))
	}
	if records[0]["latitude"] != 18.5 {
		t.Errorf("carried fields missing on the matched row: %v", records[0])
	}
	if _, carried := records[1]["latitude"]; carried {
		t.Error("the unmatched row gained a coordinate it has no claim to")
	}
}

// derive: first matching rule wins, and `then: clear:` removes fields so a
// bad coordinate cannot be used as though it were good.
func TestDeriveAppliesFirstMatchingRuleAndClears(t *testing.T) {
	spec := Spec{Pipeline: []Step{{
		ID: "quality", Uses: usesDerive, Out: "judged",
		With: With{
			Left:  "${markets}",
			Field: "coordinateQuality",
			Rules: []map[string]any{
				{"when": "$not($exists(latitude))", "value": "missing"},
				{"when": "latitude = longitude", "value": "suspect"},
				{"value": "ok"},
			},
			Then: []map[string]any{
				{"when": "coordinateQuality != 'ok'", "clear": []any{"latitude", "longitude"}},
			},
		},
	}}}
	runner, _ := testRunner(t, spec, `[]`)
	runner.rc.outputs["markets"] = []any{
		map[string]any{"id": 1, "latitude": 18.5, "longitude": 73.8},
		map[string]any{"id": 2, "longitude": 73.8},
		map[string]any{"id": 3, "latitude": 5.0, "longitude": 5.0},
	}

	records, err := runner.runSteps(context.Background())
	if err != nil {
		t.Fatalf("runSteps: %v", err)
	}
	want := []string{"ok", "missing", "suspect"}
	for i, verdict := range want {
		if got := records[i]["coordinateQuality"]; got != verdict {
			t.Errorf("record %d verdict = %v, want %q", i, got, verdict)
		}
	}
	// The suspect row's coordinates must be gone, not merely flagged.
	if _, present := records[2]["latitude"]; present {
		t.Error("a suspect coordinate survived `then: clear:` and can still be used")
	}
	if _, present := records[0]["latitude"]; !present {
		t.Error("a good coordinate was cleared")
	}
}

// A rule set with no default must fail loudly rather than leave the field
// unset, which would read downstream as a legitimate absent value.
func TestDeriveWithNoMatchingRuleFails(t *testing.T) {
	spec := Spec{Pipeline: []Step{{
		ID: "quality", Uses: usesDerive, Out: "judged",
		With: With{Left: "${markets}", Field: "verdict",
			Rules: []map[string]any{{"when": "false", "value": "never"}}},
	}}}
	runner, _ := testRunner(t, spec, `[]`)
	runner.rc.outputs["markets"] = []any{map[string]any{"id": 1}}

	if _, err := runner.runSteps(context.Background()); err == nil {
		t.Fatal("a record matching no rule was given no verdict and no error")
	}
}

// An expression that cannot be evaluated is an error, never a silent false.
// A predicate that quietly reads false is how an exclusion stops excluding.
func TestBrokenExpressionIsAnErrorNotFalse(t *testing.T) {
	spec := Spec{Pipeline: []Step{{
		ID: "quality", Uses: usesDerive, Out: "judged",
		With: With{Left: "${markets}", Field: "verdict",
			Rules: []map[string]any{{"when": "this is not ( valid jsonata", "value": "x"}, {"value": "ok"}}},
	}}}
	runner, _ := testRunner(t, spec, `[]`)
	runner.rc.outputs["markets"] = []any{map[string]any{"id": 1}}

	_, err := runner.runSteps(context.Background())
	if err == nil {
		t.Fatal("an unparseable expression was treated as false and the record kept going")
	}
}

func TestUnknownPrimitiveIsRefused(t *testing.T) {
	spec := Spec{Pipeline: []Step{{ID: "x", Uses: "http.post", Out: "x"}}}
	runner, _ := testRunner(t, spec, `[]`)

	_, err := runner.runSteps(context.Background())
	if err == nil {
		t.Fatal("an unimplemented primitive was accepted")
	}
	if !strings.Contains(err.Error(), "http.post") {
		t.Errorf("error %q does not name the primitive", err)
	}
}

// The runner must pass the loop variable into the request, or every iteration
// asks the upstream the same question.
func TestForEachPassesTheLoopVariableIntoTheRequest(t *testing.T) {
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Query().Get("stateCode"))
		_, _ = w.Write([]byte(`[{"marketId":1}]`))
	}))
	defer upstream.Close()

	cache, _ := newExprCache()
	client := NewClient(upstream.URL).WithErrorRules(testErrorRules())
	client.http = upstream.Client()

	spec := Spec{Pipeline: []Step{{
		ID: "rows", Uses: usesHTTPGet, Out: "rows",
		ForEach: "${states}", As: "state",
		With: With{Path: "/rows", Mapping: "mappings/rows.yaml",
			Local: map[string]string{"stateCode": "${state.code}"}},
	}}}
	runner := &stepRunner{
		spec: spec, cache: cache, client: client, mapper: passthroughMapper{},
		log: slog.New(slog.DiscardHandler), counters: map[string]int{},
		rc: newRunContext(map[string]string{}, "tok"),
	}
	runner.rc.outputs["states"] = []any{
		map[string]any{"code": "MH"}, map[string]any{"code": "KA"},
	}

	if _, err := runner.runSteps(context.Background()); err != nil {
		t.Fatalf("runSteps: %v", err)
	}
	if fmt.Sprint(seen) != "[MH KA]" {
		t.Errorf("upstream saw stateCode %v, want [MH KA]", seen)
	}
}

// The token must reach a request through ${auth.token} without the file
// naming a credential anywhere.
func TestAuthTokenIsAvailableToSteps(t *testing.T) {
	runner, _ := testRunner(t, Spec{}, `[]`)
	value, err := runner.rc.lookup("auth.token")
	if err != nil {
		t.Fatalf("auth.token: %v", err)
	}
	if value != "tok-fake" {
		t.Errorf("auth.token = %v", value)
	}

	// And before an exchange, using it is an error rather than an empty string
	// silently reaching the upstream as no credential at all.
	empty := newRunContext(map[string]string{}, "")
	if _, err := empty.lookup("auth.token"); err == nil {
		t.Error("${auth.token} resolved to empty before a token was exchanged")
	}
}

func TestInterpolationOfAnUnknownReferenceIsAnError(t *testing.T) {
	rc := newRunContext(map[string]string{"known": "yes"}, "tok")
	if _, err := rc.interpolate("${inputs.known}"); err != nil {
		t.Fatalf("a declared input failed to resolve: %v", err)
	}
	if _, err := rc.interpolate("${inputs.typo}"); err == nil {
		t.Error("an undeclared input interpolated to empty instead of failing")
	}
	if _, err := rc.interpolate("${nosuchstep.field}"); err == nil {
		t.Error("an unknown step output interpolated to empty instead of failing")
	}
}

// Numbers arrive from JSON as float64. A marketId rendered "101.0" is
// rejected by the upstream as a bad identifier.
func TestInterpolationRendersWholeNumbersWithoutADecimalPoint(t *testing.T) {
	rc := newRunContext(map[string]string{}, "tok")
	rc.outputs["row"] = map[string]any{"marketId": float64(101), "ratio": 1.5}

	got, err := rc.interpolate("id=${row.marketId} ratio=${row.ratio}")
	if err != nil {
		t.Fatalf("interpolate: %v", err)
	}
	if got != "id=101 ratio=1.5" {
		t.Errorf("interpolate = %q, want %q", got, "id=101 ratio=1.5")
	}
}

func TestErrNoUpstreamDataStillClassifiesWhenWrapped(t *testing.T) {
	wrapped := fmt.Errorf("state MH: %w", ErrNoUpstreamData)
	if !errors.Is(wrapped, ErrNoUpstreamData) {
		t.Fatal("wrapping broke the sentinel; a quiet state would read as an outage")
	}
}

// A ${...} inside a predicate must be substituted as a LITERAL.
//
// Raw substitution turns `${inputs.mode} = 'skip'` into `skip = 'skip'`, where
// the bare `skip` is a PATH into the record rather than a string. The record
// has no such field, so the comparison is false, so the rule silently never
// fires -- which is how a configured option stops taking effect with nothing
// to see. Measured against the pinned jsonata build, not assumed.
func TestPredicateSubstitutesInterpolationAsALiteral(t *testing.T) {
	spec := Spec{Pipeline: []Step{{
		ID: "quality", Uses: usesDerive, Out: "judged",
		With: With{Left: "${markets}", Field: "verdict",
			Rules: []map[string]any{
				{"when": "${inputs.mode} = 'skip'", "value": "mode-matched"},
				{"value": "mode-did-not-match"},
			}},
	}}}
	runner, _ := testRunner(t, spec, `[]`)
	runner.rc = newRunContext(map[string]string{"mode": "skip"}, "tok")
	runner.rc.outputs["markets"] = []any{map[string]any{"id": 1}}

	records, err := runner.runSteps(context.Background())
	if err != nil {
		t.Fatalf("runSteps: %v", err)
	}
	if got := records[0]["verdict"]; got != "mode-matched" {
		t.Errorf("verdict = %v, want mode-matched -- the interpolated value was "+
			"compared as a record path instead of a string, so the rule never fired", got)
	}
}

// Every primitive that names a mapping must resolve it the SAME way:
// relative to the pipeline file's URL, like a link in a page.
//
// Three call sites once resolved `mapping:` two different ways, and two of
// them used filepath.Join on a URL -- which collapses "http://host" to
// "http:/host", an address nothing can fetch.
func TestMappingRefIsResolvedTheSameWayEverywhere(t *testing.T) {
	runner := &stepRunner{mappingBase: "http://127.0.0.1:53211/testdata/minimal.yaml"}
	got, err := runner.mappingRef("mappings/things.yaml")
	if err != nil {
		t.Fatalf("mappingRef: %v", err)
	}
	if want := "http://127.0.0.1:53211/testdata/mappings/things.yaml"; got != want {
		t.Errorf("mappingRef = %q, want %q", got, want)
	}
}

// A const step is how a pipeline with no upstream produces its collection:
// the records are written in the file, and the step hands them on unchanged.
func TestConstEmitsTheRecordsTheFileDeclares(t *testing.T) {
	spec := Spec{Pipeline: []Step{
		{ID: "resources", Uses: usesConst, Out: "collection", With: With{Records: []map[string]any{
			{"id": "point-forecast"},
			{"id": "district-forecast"},
		}}},
	}}
	runner, upstream := testRunner(t, spec, `[]`)
	upstream.Close() // a const step must not need the upstream at all

	records, err := runner.runSteps(context.Background())
	if err != nil {
		t.Fatalf("runSteps: %v", err)
	}
	if len(records) != 2 || records[0]["id"] != "point-forecast" || records[1]["id"] != "district-forecast" {
		t.Fatalf("records = %v, want the two declared, in order", records)
	}

	// Copies, not the spec's own maps: catalog rendering writes resourceId
	// into each record, and that must not leak back into the parsed file.
	records[0]["resourceId"] = "mutated"
	if _, leaked := spec.Pipeline[0].With.Records[0]["resourceId"]; leaked {
		t.Error("a const step handed out the spec's own record maps; a later edit wrote back into the file")
	}
}

// A const step with nothing in it is refused: it would walk cleanly into an
// empty catalog build and read as "this provider has nothing".
func TestConstWithNoRecordsIsRefused(t *testing.T) {
	spec := Spec{Pipeline: []Step{{ID: "resources", Uses: usesConst, Out: "collection"}}}
	runner, _ := testRunner(t, spec, `[]`)

	_, err := runner.runSteps(context.Background())
	if err == nil || !strings.Contains(err.Error(), "records") {
		t.Fatalf("err = %v, want a refusal naming `with.records`", err)
	}
}

// concat appends named collections in the order the file lists them, so one
// catalog block can build two kinds of catalog from one run.
func TestConcatAppendsCollectionsInOrder(t *testing.T) {
	spec := Spec{Pipeline: []Step{
		{ID: "a", Uses: usesConst, Out: "markets", With: With{Records: []map[string]any{{"id": "m1"}, {"id": "m2"}}}},
		{ID: "b", Uses: usesConst, Out: "prices", With: With{Records: []map[string]any{{"id": "p1"}}}},
		{ID: "both", Uses: usesConcat, Out: "collection", With: With{Of: []string{"${markets}", "${prices}"}}},
	}}
	runner, _ := testRunner(t, spec, `[]`)

	records, err := runner.runSteps(context.Background())
	if err != nil {
		t.Fatalf("runSteps: %v", err)
	}
	var ids []string
	for _, record := range records {
		ids = append(ids, record["id"].(string))
	}
	if strings.Join(ids, ",") != "m1,m2,p1" {
		t.Fatalf("ids = %v, want m1,m2,p1", ids)
	}

	// Copies: the catalog builder writes resourceId into each record, and
	// that must not reach back into the steps that produced them.
	records[0]["resourceId"] = "mutated"
	if _, leaked := runner.rc.outputs["markets"].([]map[string]any)[0]["resourceId"]; leaked {
		t.Error("concat handed out the source collection's own maps")
	}
}

// A concat naming fewer than two collections is a mistake, not a no-op.
func TestConcatNeedsTwoCollections(t *testing.T) {
	spec := Spec{Pipeline: []Step{
		{ID: "a", Uses: usesConst, Out: "markets", With: With{Records: []map[string]any{{"id": "m1"}}}},
		{ID: "both", Uses: usesConcat, With: With{Of: []string{"${markets}"}}},
	}}
	runner, _ := testRunner(t, spec, `[]`)

	_, err := runner.runSteps(context.Background())
	if err == nil || !strings.Contains(err.Error(), "of") {
		t.Fatalf("err = %v, want a refusal naming `with.of`", err)
	}
}

// With no upstream there is no client. An HTTP step reaching the runner
// anyway (the schema should have refused it) must fail by name, not panic.
func TestHTTPStepWithoutAnUpstreamIsRefused(t *testing.T) {
	for _, uses := range []string{usesHTTPGet, usesHTTPPost} {
		t.Run(uses, func(t *testing.T) {
			spec := Spec{Pipeline: []Step{
				{ID: "fetch", Uses: uses, Out: "rows",
					With: With{Path: "/things", Mapping: "mappings/things.yaml"}},
			}}
			runner, _ := testRunner(t, spec, `[]`)
			runner.client = nil

			_, err := runner.runSteps(context.Background())
			if err == nil || !strings.Contains(err.Error(), "upstream") {
				t.Fatalf("err = %v, want a refusal naming the missing upstream", err)
			}
		})
	}
}

// testErrorRules are the classifications a pipeline file declares, in the
// shape mandi's own upstream.errors block uses.
//
// Passed explicitly by every test that builds a client, because the engine no
// longer knows any upstream's wording: without declared rules nothing is a
// quiet result, which is the correct default and the thing these tests would
// otherwise silently stop covering.
func testErrorRules() []ErrorRule {
	return []ErrorRule{
		{When: &ErrorMatch{Status: 400, BodyContains: "No data available."}, Classify: classifyEmpty},
		{Default: classifyTransport},
	}
}

// postRunner is a runner whose upstream records the POST it receives and
// answers with body.
func postRunner(t *testing.T, spec Spec, status int, body string) (*stepRunner, *postSeen) {
	t.Helper()
	seen := &postSeen{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.method, seen.path, seen.query = r.Method, r.URL.Path, r.URL.Query().Get("token")
		seen.auth, seen.contentType = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		seen.body, _ = io.ReadAll(r.Body)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(upstream.Close)

	runner, _ := testRunner(t, spec, `[]`)
	runner.client = NewClient(upstream.URL)
	runner.client.http = upstream.Client()
	return runner, seen
}

type postSeen struct {
	method, path, query, auth, contentType string
	body                                   []byte
}

// http.post builds its JSON body from the mapping's request half, carries the
// token where the upstream's auth says, and runs the response half over the
// answer -- the three things a search-style upstream needs.
func TestHTTPPostSendsTheMappedBodyAndReadsTheAnswer(t *testing.T) {
	spec := Spec{Pipeline: []Step{{
		ID: "search", Uses: usesHTTPPost, Out: "results",
		With: With{Path: "/v1/search", Mapping: "mappings/search.yaml",
			Local: map[string]string{"token": "${auth.token}", "dataset": "regions"}},
	}}}
	for _, place := range []string{"header", "query"} {
		t.Run(place, func(t *testing.T) {
			runner, seen := postRunner(t, spec, http.StatusOK, `[{"id":"r1"},{"id":"r2"}]`)
			credential := Credential{Value: "tok-fake", CarriedAs: "query", Name: "token"}
			if place == "header" {
				credential = Credential{Value: "tok-fake", CarriedAs: "header", Name: "Authorization", Prefix: "Bearer "}
			}
			runner.client.WithCredential(credential)

			records, err := runner.runSteps(context.Background())
			if err != nil {
				t.Fatalf("runSteps: %v", err)
			}
			if len(records) != 2 || records[1]["id"] != "r2" {
				t.Fatalf("records = %v, want the two the upstream answered", records)
			}
			if seen.method != http.MethodPost || seen.path != "/v1/search" || seen.contentType != "application/json" {
				t.Errorf("request = %s %s (%s), want a JSON POST to /v1/search", seen.method, seen.path, seen.contentType)
			}
			var sent map[string]any
			if err := json.Unmarshal(seen.body, &sent); err != nil || sent["dataset"] != "regions" {
				t.Errorf("body = %s, want the mapping's request half", seen.body)
			}
			switch place {
			case "header":
				if seen.auth != "Bearer tok-fake" || seen.query != "" {
					t.Errorf("auth header %q, query %q; want the token in the header only", seen.auth, seen.query)
				}
			case "query":
				if seen.query != "tok-fake" || seen.auth != "" {
					t.Errorf("auth header %q, query %q; want the token in the query only", seen.auth, seen.query)
				}
			}
		})
	}
}

// An answer that is not a JSON array is refused before the response half
// runs: an error object would otherwise decode into one phantom record.
func TestHTTPPostRefusesAnAnswerThatIsNotAnArray(t *testing.T) {
	spec := Spec{Pipeline: []Step{{ID: "search", Uses: usesHTTPPost, Out: "results",
		With: With{Path: "/v1/search", Mapping: "mappings/search.yaml"}}}}
	runner, _ := postRunner(t, spec, http.StatusOK, `{"message":"no data found"}`)

	_, err := runner.runSteps(context.Background())
	if err == nil || !strings.Contains(err.Error(), "array was expected") {
		t.Fatalf("err = %v, want a refusal of the non-array answer", err)
	}
}

// A failing upstream is classified like any other call, so onError can tell
// a quiet "no rows" from an outage.
func TestHTTPPostClassifiesAFailure(t *testing.T) {
	spec := Spec{Pipeline: []Step{{ID: "search", Uses: usesHTTPPost, Out: "results",
		With: With{Path: "/v1/search", Mapping: "mappings/search.yaml"}}}}
	runner, _ := postRunner(t, spec, http.StatusInternalServerError, `{"error":"down"}`)

	if _, err := runner.runSteps(context.Background()); err == nil {
		t.Fatal("a 500 from the upstream was accepted")
	}
}

func TestHTTPPostNeedsPathAndMapping(t *testing.T) {
	runner, _ := postRunner(t, Spec{Pipeline: []Step{{ID: "search", Uses: usesHTTPPost}}}, http.StatusOK, `[]`)
	if _, err := runner.runSteps(context.Background()); err == nil || !strings.Contains(err.Error(), "path") {
		t.Fatalf("err = %v, want a refusal naming path and mapping", err)
	}
}

// filter keeps the records its predicate holds for, in order, and drops the
// rest -- the cleanup a join or dedupe needs before a defective row can
// become a key.
func TestFilterKeepsOnlyMatchingRecords(t *testing.T) {
	spec := Spec{Pipeline: []Step{
		{ID: "rows", Uses: usesConst, Out: "rows", With: With{Records: []map[string]any{
			{"id": "a", "status": "ACTIVE"},
			{"id": "", "status": "ACTIVE"},
			{"id": "c", "status": "WITHDRAWN"},
			{"id": "d", "status": "ACTIVE"},
		}}},
		{ID: "active", Uses: usesFilter, Out: "active",
			With: With{When: "id != '' and status != 'WITHDRAWN'"}},
	}}
	runner, _ := testRunner(t, spec, `[]`)

	records, err := runner.runSteps(context.Background())
	if err != nil {
		t.Fatalf("runSteps: %v", err)
	}
	if len(records) != 2 || records[0]["id"] != "a" || records[1]["id"] != "d" {
		t.Fatalf("records = %v, want a and d, in order", records)
	}
}

func TestFilterRefusesAMissingOrBrokenPredicate(t *testing.T) {
	rows := Step{ID: "rows", Uses: usesConst, Out: "rows", With: With{Records: []map[string]any{{"id": "a"}}}}
	for name, when := range map[string]string{"missing": "", "broken": "id = = 'a'"} {
		t.Run(name, func(t *testing.T) {
			runner, _ := testRunner(t, Spec{Pipeline: []Step{rows,
				{ID: "active", Uses: usesFilter, Out: "active", With: With{When: when}}}}, `[]`)
			if _, err := runner.runSteps(context.Background()); err == nil {
				t.Fatalf("a %s predicate was accepted", name)
			}
		})
	}
}

// localMapper answers the response half with every record stamped with the
// _local values it was given, so a test can see both reach the mapping.
type localMapper struct{ ref string }

func (m *localMapper) Verify(context.Context, string, any) error { return nil }

func (m *localMapper) Transform(_ context.Context, ref string, _ definition.Direction, in any) ([]byte, error) {
	m.ref = ref
	input, _ := in.(map[string]any)
	local, _ := input["_local"].(map[string]any)
	var out []map[string]any
	records, _ := input["response"].([]map[string]any)
	for _, record := range records {
		stamped := map[string]any{}
		for k, v := range record {
			stamped[k] = v
		}
		for k, v := range local {
			stamped[k] = v
		}
		out = append(out, stamped)
	}
	return json.Marshal(out)
}

// transform runs a mapping over the collection with no HTTP call: the records
// and the step's local values reach the mapping, and its output is the step's.
func TestTransformRunsTheMappingOverTheCollection(t *testing.T) {
	spec := Spec{Pipeline: []Step{
		{ID: "rows", Uses: usesConst, Out: "rows", With: With{Records: []map[string]any{{"id": "a"}, {"id": "b"}}}},
		{ID: "stamped", Uses: usesTransform, Out: "stamped",
			With: With{Mapping: "mappings/enrich.yaml", Local: map[string]string{"sourceId": "example-source"}}},
	}}
	runner, upstream := testRunner(t, spec, `[]`)
	upstream.Close() // a transform must not need the upstream
	mapper := &localMapper{}
	runner.mapper = mapper
	runner.mappingBase = "http://127.0.0.1:1/pipeline.yaml"

	records, err := runner.runSteps(context.Background())
	if err != nil {
		t.Fatalf("runSteps: %v", err)
	}
	if len(records) != 2 || records[0]["sourceId"] != "example-source" || records[1]["id"] != "b" {
		t.Fatalf("records = %v, want both records stamped with the local value", records)
	}
	if mapper.ref != "http://127.0.0.1:1/mappings/enrich.yaml" {
		t.Errorf("mapping ref = %q, want it resolved beside the pipeline file", mapper.ref)
	}
}

func TestTransformNeedsAMapping(t *testing.T) {
	spec := Spec{Pipeline: []Step{
		{ID: "rows", Uses: usesConst, Out: "rows", With: With{Records: []map[string]any{{"id": "a"}}}},
		{ID: "stamped", Uses: usesTransform, Out: "stamped"},
	}}
	runner, _ := testRunner(t, spec, `[]`)
	if _, err := runner.runSteps(context.Background()); err == nil || !strings.Contains(err.Error(), "mapping") {
		t.Fatalf("err = %v, want a refusal naming the missing mapping", err)
	}
}

// renderScalar is the text a ${...} becomes -- a group key, a catalog slug,
// a query parameter. Whole numbers from JSON (float64) must render without a
// ".0", or a marketId reaches an upstream as "101.0" and a slug as "MH.0".
func TestRenderScalarRendersEachJSONType(t *testing.T) {
	for want, value := range map[string]any{
		"":          nil,
		"MH":        "MH",
		"true":      true,
		"101":       float64(101),
		"18.5204":   18.5204,
		"42":        42,
		`["a","b"]`: []any{"a", "b"},
		`{"k":1}`:   map[string]any{"k": 1},
	} {
		if got := renderScalar(value); got != want {
			t.Errorf("renderScalar(%#v) = %q, want %q", value, got, want)
		}
	}
}

// A step without `out:` must still feed the next one.
//
// previousOutput used to scan the whole pipeline for the last step that named
// an output, so a step with no `out:` had its work DISCARDED and the step
// after it silently read an older collection. A filter followed by a dedupe
// is the ordinary shape that hits this: the filter runs, removes nothing from
// what the dedupe sees, and the run publishes the rows the file excluded.
func TestAStepWithoutOutStillFeedsTheNext(t *testing.T) {
	spec := Spec{Pipeline: []Step{
		// The filter names its own input and deliberately has NO `out:`.
		{ID: "drop", Uses: usesFilter, With: With{Left: "${rows}", When: "keep = true"}},
		{ID: "final", Uses: usesDedupe, Out: "collection", With: With{Key: "id"}},
	}}
	runner, _ := testRunner(t, spec, `[]`)
	runner.rc.outputs["rows"] = []any{
		map[string]any{"id": 1, "keep": true},
		map[string]any{"id": 2, "keep": false},
		map[string]any{"id": 3, "keep": true},
	}

	records, err := runner.runSteps(context.Background())
	if err != nil {
		t.Fatalf("runSteps: %v", err)
	}
	if len(records) != 2 {
		t.Errorf("got %d records, want 2 -- the filter's work was discarded and the "+
			"excluded row reached the catalog", len(records))
	}
	for _, record := range records {
		if record["keep"] != true {
			t.Errorf("a record the filter excluded survived: %v", record)
		}
	}
}

// dedupe turns every record missing its key into the SAME key, so they all
// collapse into one. Thousands of rows can vanish with nothing said.
func TestDedupeRefusesRecordsMissingTheKey(t *testing.T) {
	spec := Spec{Pipeline: []Step{
		{ID: "dedupe", Uses: usesDedupe, Out: "collection", With: With{Key: "id"}},
	}}
	runner, _ := testRunner(t, spec, `[]`)
	runner.rc.outputs["seeded"] = nil
	runner.lastOutput = []any{
		map[string]any{"id": 1},
		map[string]any{"name": "no id here"},
		map[string]any{"name": "nor here"},
	}

	_, err := runner.runSteps(context.Background())
	if err == nil {
		t.Fatal("two records with no key collapsed into one and the run carried on")
	}
	if !strings.Contains(err.Error(), "id") {
		t.Errorf("error %q does not name the key that was missing", err)
	}
}

// filter evaluated its predicate without interpolating, so a ${...} inside it
// was compared as a record path. The filter then kept everything, silently --
// the same bug derive and exclude already had fixed.
func TestFilterSubstitutesInterpolationAsALiteral(t *testing.T) {
	spec := Spec{Pipeline: []Step{
		{ID: "drop", Uses: usesFilter, Out: "kept",
			With: With{Left: "${rows}", When: "region = ${inputs.onlyRegion}"}},
	}}
	runner, _ := testRunner(t, spec, `[]`)
	runner.rc = newRunContext(map[string]string{"onlyRegion": "AA"}, "tok")
	runner.rc.outputs["rows"] = []any{
		map[string]any{"id": 1, "region": "AA"},
		map[string]any{"id": 2, "region": "BB"},
	}

	records, err := runner.runSteps(context.Background())
	if err != nil {
		t.Fatalf("runSteps: %v", err)
	}
	if len(records) != 1 || records[0]["region"] != "AA" {
		t.Errorf("filter kept %v; the interpolated value was compared as a record path, "+
			"so the predicate never matched properly", records)
	}
}

// Presence is not identity. An explicit null is PRESENT and renders to the
// same empty string an absent key does, so records carrying "id": null would
// still all collapse into one survivor.
func TestDedupeRefusesNullAndBlankKeys(t *testing.T) {
	for name, records := range map[string][]any{
		"an explicit null": {
			map[string]any{"id": 1},
			map[string]any{"id": nil},
			map[string]any{"id": nil},
		},
		"a blank string": {
			map[string]any{"id": 1},
			map[string]any{"id": ""},
			map[string]any{"id": "   "},
		},
	} {
		t.Run(name, func(t *testing.T) {
			spec := Spec{Pipeline: []Step{
				{ID: "dedupe", Uses: usesDedupe, Out: "collection", With: With{Key: "id"}},
			}}
			runner, _ := testRunner(t, spec, `[]`)
			runner.lastOutput = records

			if _, err := runner.runSteps(context.Background()); err == nil {
				t.Error("records with no usable key collapsed into one and the run carried on")
			}
		})
	}
}

// A step skipped by `when:` with no `else:` is a NO-OP: the collection flows
// past it. Yielding nil instead made the next implicit consumer fail with
// "no step before it produced one" -- an error about the runner's bookkeeping
// rather than anything the file got wrong.
func TestASkippedStepPassesTheCollectionThrough(t *testing.T) {
	spec := Spec{Pipeline: []Step{
		{ID: "seed", Uses: usesFilter, Out: "seeded", With: With{Left: "${rows}", When: "true"}},
		// Skipped, and names no else.
		{ID: "maybe", Uses: usesFilter, When: "${inputs.enabled} = 'yes'", With: With{When: "true"}},
		{ID: "final", Uses: usesDedupe, Out: "collection", With: With{Key: "id"}},
	}}
	runner, _ := testRunner(t, spec, `[]`)
	runner.rc = newRunContext(map[string]string{"enabled": "no"}, "tok")
	runner.rc.outputs["rows"] = []any{
		map[string]any{"id": 1},
		map[string]any{"id": 2},
	}

	records, err := runner.runSteps(context.Background())
	if err != nil {
		t.Fatalf("a skipped step broke the chain: %v", err)
	}
	if len(records) != 2 {
		t.Errorf("got %d records, want 2 -- the skipped step swallowed the collection", len(records))
	}
}

// A step skipped with no `else:` must not bind the PREVIOUS step's collection
// to its own `out:` name.
//
// The pass-through itself is deliberate: the collection flows past a skipped
// step so the next implicit consumer still has one. But binding it to the
// skipped step's name makes ${enriched} resolve to un-enriched data, and every
// later step reads as if the skipped one had run.
func TestASkippedStepDoesNotClaimThePreviousStepsOutput(t *testing.T) {
	spec := Spec{Pipeline: []Step{
		{ID: "fetch", Uses: usesHTTPGet, Out: "rows",
			With: With{Path: "/things", Mapping: "mappings/things.yaml"}},
		// Never runs, and declares no else.
		{ID: "enrich", Uses: usesDedupe, Out: "enriched",
			When: "1 = 2", With: With{Key: "id"}},
	}}
	runner, _ := testRunner(t, spec, `[{"id":1},{"id":2}]`)

	records, err := runner.runSteps(context.Background())
	if err != nil {
		t.Fatalf("runSteps: %v", err)
	}
	// The pass-through still happened: the run has a collection.
	if len(records) != 2 {
		t.Fatalf("the collection did not flow past the skipped step: got %d records", len(records))
	}
	if _, bound := runner.rc.outputs["enriched"]; bound {
		t.Error("a skipped step bound the previous step's collection to its own out: name")
	}
	if count, recorded := runner.counters["step:enrich"]; recorded {
		t.Errorf("a skipped step recorded %d records as its own", count)
	}
}

// countingAuth hands out a fresh token on every Prepare, so a test can tell
// the dead token from its replacement.
type countingAuth struct{ calls int }

func (a *countingAuth) RequiredInputs() []string { return nil }

func (a *countingAuth) Prepare(context.Context, *Client, *runContext) (Credential, error) {
	a.calls++
	return Credential{Value: fmt.Sprintf("tok-%d", a.calls), CarriedAs: "query", Name: "token"}, nil
}

// reauthRunner is a runner over an upstream that rejects every token accept
// reports true for. The step loops over n items and carries ${auth.token}.
func reauthRunner(t *testing.T, accept func(token string) bool, n int, onError map[string]StepOutcome,
	log *slog.Logger) (*stepRunner, *countingAuth) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !accept(r.URL.Query().Get("token")) {
			http.Error(w, `{"error":"token expired"}`, http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`[{"marketId":1}]`))
	}))
	t.Cleanup(upstream.Close)

	cache, _ := newExprCache()
	client := NewClient(upstream.URL).WithErrorRules([]ErrorRule{
		{When: &ErrorMatch{Status: []any{401, 403}}, Classify: classifyReauth},
		{Default: classifyTransport},
	})
	client.http = upstream.Client()

	spec := Spec{
		Upstream: Upstream{Auth: Auth{Token: AuthTokenSpec{ReexchangeOn: []int{401, 403}}}},
		Pipeline: []Step{{
			ID: "rows", Uses: usesHTTPGet, Out: "rows",
			ForEach: "${states}", As: "state", Concurrency: 1,
			With: With{Path: "/rows", Mapping: "mappings/rows.yaml",
				Local: map[string]string{"token": "${auth.token}", "stateCode": "${state.code}"}},
			OnError: onError,
		}},
	}
	auth := &countingAuth{}
	runner := &stepRunner{
		spec: spec, cache: cache, client: client, mapper: passthroughMapper{},
		log: log, counters: map[string]int{}, auth: auth,
		rc: newRunContext(map[string]string{}, "tok-0"),
	}
	states := make([]any, 0, n)
	for i := 0; i < n; i++ {
		states = append(states, map[string]any{"code": fmt.Sprintf("S%d", i)})
	}
	runner.rc.outputs["states"] = states
	return runner, auth
}

// A dead token is replaced once and the call retried, and every later item
// carries the replacement rather than dying on the old one.
func TestHTTPStepReexchangesADeadTokenAndRetries(t *testing.T) {
	runner, auth := reauthRunner(t, func(token string) bool { return token != "tok-0" }, 3, nil,
		slog.New(slog.DiscardHandler))

	records, err := runner.runSteps(context.Background())
	if err != nil {
		t.Fatalf("runSteps: %v", err)
	}
	if len(records) != 3 {
		t.Errorf("got %d records, want 3", len(records))
	}
	if auth.calls != 1 || runner.counters["reauth"] != 1 {
		t.Errorf("Prepare calls = %d, reauth counter = %d; want 1 and 1", auth.calls, runner.counters["reauth"])
	}
}

// Review Focus 3: an upstream that rejects every token is re-exchanged at most
// maxReauthsPerRun times, then each failure falls through to onError -- and
// no token ever reaches the log.
func TestHTTPStepStopsReexchangingAfterTheCap(t *testing.T) {
	var buf strings.Builder
	runner, auth := reauthRunner(t, func(string) bool { return false }, 5,
		map[string]StepOutcome{"reauth": {Record: "authErrors", Continue: true}},
		slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	if _, err := runner.runSteps(context.Background()); err != nil {
		t.Fatalf("runSteps: %v; onError.reauth says continue", err)
	}
	if auth.calls != maxReauthsPerRun || runner.counters["reauth"] != maxReauthsPerRun {
		t.Errorf("Prepare calls = %d, reauth counter = %d; want %d each",
			auth.calls, runner.counters["reauth"], maxReauthsPerRun)
	}
	if runner.counters["authErrors"] != 5 {
		t.Errorf("authErrors = %d, want 5 (every item fell through to onError)", runner.counters["authErrors"])
	}
	if strings.Contains(buf.String(), "tok-") {
		t.Errorf("a token reached the log:\n%s", buf.String())
	}
}

// Each upstream call inside a forEach names the item it was made for, so a
// run stuck on one state says which.
func TestForEachCallsNameTheirItemInTheLog(t *testing.T) {
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, nil))
	runner, _ := reauthRunner(t, func(string) bool { return true }, 2, nil, log)
	runner.client.WithLogger(log)

	if _, err := runner.runSteps(context.Background()); err != nil {
		t.Fatalf("runSteps: %v", err)
	}
	for _, want := range []string{"item=S0", "item=S1"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, buf.String())
		}
	}
}

// A step whose output is not a collection says what it was, rather than
// reporting zero records -- which reads as "the upstream had nothing".
func TestProducedLogsTheRawTypeOfANonCollection(t *testing.T) {
	if got := recordsForLog("text"); got.count != 0 || got.rawType != "string" {
		t.Fatalf("recordsForLog(string) = %+v, want count 0 and rawType string", got)
	}
	if got := recordsForLog([]any{map[string]any{"a": 1}}); got.count != 1 || got.rawType != "" {
		t.Fatalf("recordsForLog(records) = %+v, want count 1 and no rawType", got)
	}
}

// ---- tests for the publish step (formerly publish_test.go) ----

// publishTestPrefix is the filename prefix the pipeline's build step writes
// (build.output.filenamePrefix in the YAML), so the tests exercise the same
// naming convention a real run produces.
const publishTestPrefix = "mandi"

// writeCatalog writes one catalog file named <prefix>-<STATE>.json, shaped
// like a real publish body so the publish step can read its catalog id back.
func writeCatalog(t *testing.T, dir, state string) {
	t.Helper()
	id := "cat-mandi-" + state
	body := `{"context":{"action":"catalog/publish"},"message":{"catalogs":[{"id":"` + id +
		`","isActive":true,"resources":[{"id":"res:mandi:1"}]}],` +
		`"publishDirectives":[{"catalogId":"` + id + `"}]}}`
	path := filepath.Join(dir, publishTestPrefix+"-"+state+".json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// fakePublisher stands in for the crawler's sink: it answers every body with
// one status and keeps what it was sent, so a test can assert what reached it
// -- or that nothing did. The HTTP call and the judgement of a real answer are
// the sink's, and tested there; what is tested here is the publish step's own
// rules.
type fakePublisher struct {
	status string

	mu      sync.Mutex
	urls    []string
	bodies  [][]byte
	retires []retireCall
}

// retireCall is one Retire the publish step asked for.
type retireCall struct{ baseURL, catalogID, descriptorName string }

func publisherAnswering(status string) *fakePublisher { return &fakePublisher{status: status} }

func (f *fakePublisher) Publish(_ context.Context, baseURL string, body []byte) Outcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.urls = append(f.urls, baseURL)
	f.bodies = append(f.bodies, body)
	return Outcome{Status: f.status, Reason: "fake"}
}

func (f *fakePublisher) Retire(_ context.Context, baseURL string, r Retirement) Outcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retires = append(f.retires, retireCall{baseURL, r.CatalogID, r.DescriptorName})
	return Outcome{CatalogID: r.CatalogID, Status: f.status, Reason: "fake"}
}

func (f *fakePublisher) retired() []retireCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]retireCall(nil), f.retires...)
}

func (f *fakePublisher) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

// testAdapter is the base address the tests hand the publish step.
const testAdapter = "http://adapter.test"

// goodSpec is the publish block the pipeline actually declares.
func goodSpec() Publish {
	return Publish{
		URL:        "${inputs.publishUrl}/publish",
		RefuseWhen: "collection.groupErrors > 0",
	}
}

func TestPublishCatalogsReportsAnAcceptedCatalogAsPublished(t *testing.T) {
	pub := publisherAnswering(StatusPublished)

	dir := t.TempDir()
	writeCatalog(t, dir, "MH")

	result, err := PublishCatalogs(context.Background(), goodSpec(),
		map[string]string{"publishUrl": testAdapter}, dir, publishTestPrefix, nil, pub)
	if err != nil {
		t.Fatalf("publishCatalogs: %v", err)
	}

	if pub.calls() != 1 {
		t.Errorf("posted %d times, want 1", pub.calls())
	}
	if len(result.Outcomes) != 1 {
		t.Fatalf("outcomes = %+v, want 1", result.Outcomes)
	}
	if result.Outcomes[0].Status != StatusPublished {
		t.Errorf("status = %q, want %q", result.Outcomes[0].Status, StatusPublished)
	}
	if result.HasFailures() {
		t.Error("HasFailures is true after an ACCEPTED result")
	}
}

func TestPublishCatalogsTreatsPartialAsAFailure(t *testing.T) {
	// A PARTIAL is a catalog indexed with resources missing -- 273 markets
	// published as 128 findable ones -- so it must never read as success.
	pub := publisherAnswering(StatusRejected)

	dir := t.TempDir()
	writeCatalog(t, dir, "MH")

	result, err := PublishCatalogs(context.Background(), goodSpec(),
		map[string]string{"publishUrl": testAdapter}, dir, publishTestPrefix, nil, pub)
	if err != nil {
		t.Fatalf("publishCatalogs: %v", err)
	}
	if !result.HasFailures() {
		t.Fatalf("HasFailures is false after a PARTIAL: %+v", result.Outcomes)
	}
	if result.Outcomes[0].Status != StatusRejected {
		t.Errorf("status = %q, want %q", result.Outcomes[0].Status, StatusRejected)
	}
}

func TestPublishCatalogsRefusesAPartialCollection(t *testing.T) {
	pub := publisherAnswering(StatusPublished)

	dir := t.TempDir()
	writeCatalog(t, dir, "MH")

	_, err := PublishCatalogs(context.Background(), goodSpec(),
		map[string]string{"publishUrl": testAdapter}, dir, publishTestPrefix, map[string]int{"groupErrors": 3}, pub)
	if err == nil {
		t.Fatal("publishCatalogs published a collection with 3 failed states")
	}
	if pub.calls() != 0 {
		t.Errorf("posted %d times after refusing, want 0", pub.calls())
	}
	if !strings.Contains(err.Error(), "3") {
		t.Errorf("error %q does not say how many states failed", err)
	}
}

func TestPublishCatalogsPublishesWhenRefuseWhenIsNotDeclared(t *testing.T) {
	pub := publisherAnswering(StatusPublished)

	dir := t.TempDir()
	writeCatalog(t, dir, "MH")

	spec := goodSpec()
	spec.RefuseWhen = ""

	if _, err := PublishCatalogs(context.Background(), spec,
		map[string]string{"publishUrl": testAdapter}, dir, publishTestPrefix, map[string]int{"groupErrors": 3}, pub); err != nil {
		t.Fatalf("publishCatalogs: %v", err)
	}
	if pub.calls() != 1 {
		t.Errorf("posted %d times, want 1", pub.calls())
	}
}

func TestPublishCatalogsNeedsAPublishURL(t *testing.T) {
	// The hint the run fills in from the pipeline's own publishUrl input.
	spec := goodSpec()
	spec.AddressHint = publishAddressHintFor(Input{Flag: "publish-url", Env: "CATALOG_PUBLISH_URL"})
	_, err := PublishCatalogs(context.Background(), spec,
		map[string]string{}, t.TempDir(), publishTestPrefix, nil, publisherAnswering(StatusPublished))
	if err == nil {
		t.Fatal("publishCatalogs accepted an empty publish address")
	}
	if !strings.Contains(err.Error(), "publishUrl") || !strings.Contains(err.Error(), "CATALOG_PUBLISH_URL") {
		t.Errorf("error %q names neither the input nor its environment variable", err)
	}
}

// TestPublishCatalogsRejectsAPublishURLItWouldIgnore: the address actually
// used comes from inputs.publishUrl, so a publish.url pointing anywhere else
// would be silently ignored -- an operator's edit taking no effect, with no
// message saying so.
func TestPublishCatalogsRejectsAPublishURLItWouldIgnore(t *testing.T) {
	spec := goodSpec()
	spec.URL = "https://somewhere.else.test/publish"

	dir := t.TempDir()
	writeCatalog(t, dir, "MH")

	_, err := PublishCatalogs(context.Background(), spec,
		map[string]string{"publishUrl": "http://127.0.0.1:1"}, dir, publishTestPrefix, nil, publisherAnswering(StatusPublished))
	if err == nil {
		t.Fatal("publishCatalogs accepted a publish.url it does not honour")
	}
	if !strings.Contains(err.Error(), "somewhere.else.test") {
		t.Errorf("error %q does not quote the ignored url", err)
	}
}

// TestPublishCatalogsRefusesAnUnresolvableRetireOld: the file's retireOld
// block is gated on ${inputs.retireOld}, an input the file never declares.
// Reading that as "off" would silently skip a retirement somebody wrote the
// block specifically to get.
func TestPublishCatalogsRefusesAnUnresolvableRetireOld(t *testing.T) {
	spec := goodSpec()
	spec.RetireOld = RetireOld{
		Enabled:        "${inputs.retireOld}",
		CatalogID:      "cat-agmarknet-mandi-prices",
		DescriptorName: "Retired: superseded by the per-state market catalogs",
	}

	dir := t.TempDir()
	writeCatalog(t, dir, "MH")

	_, err := PublishCatalogs(context.Background(), spec,
		map[string]string{"publishUrl": "http://127.0.0.1:1"}, dir, publishTestPrefix, nil, publisherAnswering(StatusPublished))
	if err == nil {
		t.Fatal("publishCatalogs accepted a retireOld gated on an undeclared input")
	}
	if !strings.Contains(err.Error(), "retireOld") {
		t.Errorf("error %q does not name the block it refused", err)
	}
}

// TestPublishCatalogsCarriesAnEnabledRetireOld proves the block reaches
// the publisher rather than being parsed and dropped: an enabled retirement
// retires the named catalog alongside publishing the current ones.
func TestPublishCatalogsCarriesAnEnabledRetireOld(t *testing.T) {
	spec := goodSpec()
	spec.RetireOld = RetireOld{
		Enabled:        "${inputs.retireOld}",
		CatalogID:      "cat-agmarknet-mandi-prices",
		DescriptorName: "Retired: superseded by the per-state market catalogs",
	}

	dir := t.TempDir()
	writeCatalog(t, dir, "MH")
	pub := publisherAnswering(StatusPublished)

	result, err := PublishCatalogs(context.Background(), spec, map[string]string{
		"publishUrl": testAdapter,
		"retireOld":  "true",
	}, dir, publishTestPrefix, nil, pub)
	if err != nil {
		t.Fatalf("publishCatalogs: %v", err)
	}
	if result.RetiredOld == nil {
		t.Fatal("retireOld was declared and enabled, but no retirement was attempted")
	}
	// The catalog was published and the old one retired, both through the
	// publisher -- which builds the retirement body, identity and all.
	want := retireCall{testAdapter, "cat-agmarknet-mandi-prices", "Retired: superseded by the per-state market catalogs"}
	if pub.calls() != 1 || len(pub.retired()) != 1 || pub.retired()[0] != want {
		t.Errorf("publisher got %d bodies and retires %+v; want the catalog, then Retire(%+v)",
			pub.calls(), pub.retired(), want)
	}
	if result.RetiredOld.CatalogID != want.catalogID {
		t.Errorf("RetiredOld = %+v, want catalogId %s", result.RetiredOld, want.catalogID)
	}
}

// The file goes to the publisher VERBATIM, with the base address -- the
// publisher appends /publish, so a rendered .../publish would double it.
func TestPublishCatalogsHandsTheFileVerbatimWithTheBaseAddress(t *testing.T) {
	dir := t.TempDir()
	writeCatalog(t, dir, "MH")
	want, _ := os.ReadFile(filepath.Join(dir, publishTestPrefix+"-MH.json"))
	pub := publisherAnswering(StatusPublished)

	result, err := PublishCatalogs(context.Background(), goodSpec(),
		map[string]string{"publishUrl": testAdapter}, dir, publishTestPrefix, nil, pub)
	if err != nil {
		t.Fatalf("PublishCatalogs: %v", err)
	}
	if pub.urls[0] != testAdapter || string(pub.bodies[0]) != string(want) {
		t.Errorf("publisher got url %q body %s; want %q and the file verbatim", pub.urls[0], pub.bodies[0], testAdapter)
	}
	if got := result.Outcomes[0]; got.CatalogID != "cat-mandi-MH" || got.Group != "MH" {
		t.Errorf("outcome = %+v, want the id from the body and the group from the filename", got)
	}
}

// Asked to publish with nothing to publish through is refused, not quietly
// turned into a build-only run.
func TestPublishCatalogsRefusesWithoutAPublisher(t *testing.T) {
	dir := t.TempDir()
	writeCatalog(t, dir, "MH")
	_, err := PublishCatalogs(context.Background(), goodSpec(),
		map[string]string{"publishUrl": testAdapter}, dir, publishTestPrefix, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "publisher") {
		t.Fatalf("err = %v, want a refusal naming the missing publisher", err)
	}
}

// The "no publish address" error names the flag and env the pipeline FILE
// declares, not a name fixed in Go: a hardcoded hint once told a pipeline to
// set a variable it never read.
func TestPublishAddressHintNamesThePipelinesOwnInput(t *testing.T) {
	hint := publishAddressHintFor(Input{Flag: "send-to", Env: "EXAMPLE_PUBLISH_URL"})
	if !strings.Contains(hint, "EXAMPLE_PUBLISH_URL") || !strings.Contains(hint, "--send-to") {
		t.Errorf("hint %q does not name the declared flag and env", hint)
	}

	spec := goodSpec()
	spec.AddressHint = hint
	_, err := PublishCatalogs(context.Background(), spec, map[string]string{}, t.TempDir(), "x", nil, publisherAnswering(StatusPublished))
	if err == nil || !strings.Contains(err.Error(), "EXAMPLE_PUBLISH_URL") {
		t.Errorf("err = %v, want it to name EXAMPLE_PUBLISH_URL", err)
	}
}

// The retirement must not go out when its replacements did not.
//
// retireOld posts a TOMBSTONE: it deactivates the old catalog, which is how
// that catalog's resources leave the network. Sending it after a run whose
// new catalogs were all rejected removes the old data and puts nothing in
// its place -- the network is left with neither. The next tick then retries
// and sends the tombstone again.
func TestRetireIsNotSentWhenEveryPublishFailed(t *testing.T) {
	dir := t.TempDir()
	writeCatalog(t, dir, "MH")

	pub := publisherAnswering(StatusRejected)
	spec := Publish{
		URL:       "${inputs.publishUrl}/publish",
		RetireOld: RetireOld{Enabled: "${inputs.retireOld}", CatalogID: "cat-old-monolith"},
	}
	resolved := map[string]string{"publishUrl": testAdapter, "retireOld": "true"}

	result, err := PublishCatalogs(context.Background(), spec, resolved, dir, publishTestPrefix, nil, pub)
	if err != nil {
		t.Fatalf("PublishCatalogs: %v", err)
	}
	if result.RetiredOld != nil {
		t.Error("the old catalog was retired although every replacement was rejected; " +
			"the network is left with neither the old data nor the new")
	}
	// One call for the catalog, no retirement.
	if got := pub.calls(); got != 1 || len(pub.retired()) != 0 {
		t.Errorf("publisher saw %d publishes and %d retires, want 1 and 0", got, len(pub.retired()))
	}
}

// And it MUST still go out on a healthy run, or the migration never completes
// and the check above is just breakage.
func TestRetireIsSentWhenEveryPublishSucceeded(t *testing.T) {
	dir := t.TempDir()
	writeCatalog(t, dir, "MH")

	pub := publisherAnswering(StatusPublished)
	spec := Publish{
		URL:       "${inputs.publishUrl}/publish",
		RetireOld: RetireOld{Enabled: "${inputs.retireOld}", CatalogID: "cat-old-monolith"},
	}
	resolved := map[string]string{"publishUrl": testAdapter, "retireOld": "true"}

	result, err := PublishCatalogs(context.Background(), spec, resolved, dir, publishTestPrefix, nil, pub)
	if err != nil {
		t.Fatalf("PublishCatalogs: %v", err)
	}
	if result.RetiredOld == nil {
		t.Fatal("a healthy run did not retire the old catalog, so the migration never completes")
	}
	if got := pub.calls(); got != 1 || len(pub.retired()) != 1 {
		t.Errorf("publisher saw %d publishes and %d retires, want 1 and 1", got, len(pub.retired()))
	}
}

func TestRefuseWhenAcceptsAThreshold(t *testing.T) {
	counter, threshold, ok := refuseWhenRule("collection.groupErrors > 3")
	if !ok || counter != "groupErrors" || threshold != 3 {
		t.Fatalf("refuseWhenRule = %q, %d, %v; want groupErrors, 3, true", counter, threshold, ok)
	}
	if _, _, ok := refuseWhenRule("collection.groupErrors >= 3"); ok {
		t.Fatal(">= was accepted; only > N is understood")
	}
}

// A pipeline that tolerates three failed groups publishes with three and
// refuses with four.
func TestPublishRefusesOnlyAboveTheThreshold(t *testing.T) {
	spec := goodSpec()
	spec.RefuseWhen = "collection.groupErrors > 3"
	resolved := map[string]string{"publishUrl": testAdapter}

	dir := t.TempDir()
	writeCatalog(t, dir, "MH")

	if _, err := PublishCatalogs(context.Background(), spec, resolved, dir, publishTestPrefix,
		map[string]int{"groupErrors": 3}, publisherAnswering(StatusPublished)); err != nil {
		t.Fatalf("three failed groups under `> 3` were refused: %v", err)
	}

	pub := publisherAnswering(StatusPublished)
	_, err := PublishCatalogs(context.Background(), spec, resolved, dir, publishTestPrefix,
		map[string]int{"groupErrors": 4}, pub)
	if err == nil || !strings.Contains(err.Error(), "refusing to publish") {
		t.Fatalf("four failed groups under `> 3`: err = %v, want a refusal", err)
	}
	if pub.calls() != 0 {
		t.Errorf("a refused collection still posted %d times", pub.calls())
	}
}

// The retireOld block is resolved into everything the retirement envelope
// carries: its defaults where the file is silent, its ${inputs.*} references
// resolved, so the deactivation reaches the same audience, under the same
// schema, as the catalogs that supersede it.
func TestRetirementResolvesTheWholeBlock(t *testing.T) {
	resolved := map[string]string{"retireOld": "true", "networkId": "oan-prod"}
	got, err := retirement(RetireOld{
		Enabled:        "${inputs.retireOld}",
		CatalogID:      "cat-old",
		DescriptorName: "Retired",
		VisibleTo:      []string{"${inputs.networkId}"},
		SchemaTypes:    []string{"https://schema.example/MandiPrice/context.jsonld"},
	}, resolved)
	if err != nil {
		t.Fatalf("retirement: %v", err)
	}
	want := Retirement{
		CatalogID: "cat-old", DescriptorName: "Retired",
		CatalogType: "REGULAR", UpdateMode: "MERGE",
		VisibleTo:   []string{"oan-prod"},
		SchemaTypes: []string{"https://schema.example/MandiPrice/context.jsonld"},
	}
	if got == nil || fmt.Sprint(*got) != fmt.Sprint(want) {
		t.Fatalf("retirement = %+v, want %+v", got, want)
	}

	full, err := retirement(RetireOld{Enabled: "${inputs.retireOld}", CatalogID: "cat-old",
		UpdateMode: "FULL", CatalogType: "MASTER"}, resolved)
	if err != nil || full.UpdateMode != "FULL" || full.CatalogType != "MASTER" {
		t.Fatalf("declared updateMode/catalogType: %+v, %v; want them passed through", full, err)
	}
}

// A retirement is a deactivation; a block saying isActive: true, or naming an
// input that does not resolve, is refused rather than quietly ignored.
func TestRetirementRefusesWhatItCannotHonour(t *testing.T) {
	resolved := map[string]string{"retireOld": "true"}
	yes := true
	for name, block := range map[string]RetireOld{
		"isActive true":         {Enabled: "${inputs.retireOld}", CatalogID: "c", IsActive: &yes},
		"an unresolved input":   {Enabled: "${inputs.retireOld}", CatalogID: "c", VisibleTo: []string{"${inputs.nope}"}},
		"an unknown updateMode": {Enabled: "${inputs.retireOld}", CatalogID: "c", UpdateMode: "REPLACE"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := retirement(block, resolved); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}
