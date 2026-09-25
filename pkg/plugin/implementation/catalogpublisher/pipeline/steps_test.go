package pipeline

// steps_test.go pins the interpreter against the control flow a pipeline file
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
	"strings"
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
	// and the catalogue block address it that way.
	if _, ok := runner.rc.outputs["rows"]; !ok {
		t.Error("the first step's output was not recorded under its `out:` name")
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
			"emptyResult":    {Record: "emptyStates", Continue: true},
			"transportError": {Record: "stateErrors", Continue: true},
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
	if runner.counters["emptyStates"] != 1 {
		t.Errorf("emptyStates = %d, want 1", runner.counters["emptyStates"])
	}
	if runner.counters["stateErrors"] != 1 {
		t.Errorf("stateErrors = %d, want 1 -- a 500 is an outage, not a quiet state",
			runner.counters["stateErrors"])
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

// Every primitive that names a mapping must resolve it the SAME way.
//
// Three call sites once resolved `mapping:` two different ways, and two of
// them used filepath.Join on a URL -- which collapses "http://host" to
// "http:/host", an address nothing can fetch. http.post and transform were
// therefore broken for every mapping, not merely for a prefixed one, and
// nothing said so because no test named a mapping from those steps.
func TestMappingRefIsResolvedTheSameWayEverywhere(t *testing.T) {
	runner := &stepRunner{mappingBase: "http://127.0.0.1:53211"}

	// Both spellings a file might use must land on the same served ref: a
	// mapping is written relative to the pipeline file, and served from the
	// root of that directory.
	for _, spelling := range []string{"mappings/things.yaml", "things.yaml"} {
		got := runner.mappingRef(spelling)
		if want := "http://127.0.0.1:53211/things.yaml"; got != want {
			t.Errorf("mappingRef(%q) = %q, want %q", spelling, got, want)
		}
	}

	// And the scheme must survive. "http:/" is the failure this guards.
	if ref := runner.mappingRef("mappings/x.yaml"); !strings.HasPrefix(ref, "http://") {
		t.Errorf("mappingRef produced %q -- the URL scheme was mangled", ref)
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
// empty catalogue build and read as "this provider has nothing".
func TestConstWithNoRecordsIsRefused(t *testing.T) {
	spec := Spec{Pipeline: []Step{{ID: "resources", Uses: usesConst, Out: "collection"}}}
	runner, _ := testRunner(t, spec, `[]`)

	_, err := runner.runSteps(context.Background())
	if err == nil || !strings.Contains(err.Error(), "records") {
		t.Fatalf("err = %v, want a refusal naming `with.records`", err)
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
			runner.client.tokenPlace = place

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
	runner.mappingBase = "http://127.0.0.1:1"

	records, err := runner.runSteps(context.Background())
	if err != nil {
		t.Fatalf("runSteps: %v", err)
	}
	if len(records) != 2 || records[0]["sourceId"] != "example-source" || records[1]["id"] != "b" {
		t.Fatalf("records = %v, want both records stamped with the local value", records)
	}
	if mapper.ref != "http://127.0.0.1:1/enrich.yaml" {
		t.Errorf("mapping ref = %q, want the served mapping, prefix stripped", mapper.ref)
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

// renderScalar is the text a ${...} becomes -- a group key, a catalogue slug,
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
