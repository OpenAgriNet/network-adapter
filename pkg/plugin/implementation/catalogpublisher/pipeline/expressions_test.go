package pipeline

// expressions_test.go pins a load-bearing fact about the JSONata library:
// whether two INDEPENDENT evaluators, each with its own instance and its own
// compiled expressions, share mutable state.
//
// The answer decides how wide the lock guarding evaluation has to be -- see
// jsonmapper.Evaluating, which this package's evaluate() takes. If the
// library keeps evaluation state in package-level variables, then a lock
// scoped to this package alone is not sufficient -- the publish sweep runs
// in the adapter process beside reqmapper on live traffic, so the two would
// evaluate concurrently.
//
// Run it with -race. Without -race it proves almost nothing.

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsonata-go/jsonata"
)

// Two separate instances, separate expressions, hammered concurrently with NO
// lock between them.
//
// This is the shape the reviewer's concern describes: pipeline's exprCache in
// one goroutine and jsonmapper's compiled mapping in another, each holding
// only its own package's lock -- which, between the two of them, is no lock at
// all.
func TestTwoIndependentEvaluatorsDoNotShareState(t *testing.T) {
	const goroutines = 8
	const iterations = 200

	// Distinct expressions and distinct inputs, so a leak between them shows
	// up as a wrong ANSWER as well as a race report.
	cases := []struct {
		expr  string
		input map[string]any
		want  float64
	}{
		{"a + b", map[string]any{"a": 1, "b": 2}, 3},
		{"x * y", map[string]any{"x": 10, "y": 7}, 70},
		{"$sum(values)", map[string]any{"values": []any{1, 2, 3, 4}}, 10},
		{"$count(items)", map[string]any{"items": []any{1, 2, 3}}, 3},
	}

	var wg sync.WaitGroup
	errs := make(chan error, goroutines*iterations)

	for g := 0; g < goroutines; g++ {
		tc := cases[g%len(cases)]
		wg.Add(1)
		go func() {
			defer wg.Done()

			// Its OWN instance and its OWN compiled expression, exactly as a
			// second package would have.
			instance, err := jsonata.OpenLatest()
			if err != nil {
				errs <- err
				return
			}
			compiled, err := instance.Compile(tc.expr, false)
			if err != nil {
				errs <- err
				return
			}
			document, err := json.Marshal(tc.input)
			if err != nil {
				errs <- err
				return
			}

			for i := 0; i < iterations; i++ {
				out, err := compiled.Evaluate(document, nil)
				if err != nil {
					errs <- err
					return
				}
				var got float64
				if err := json.Unmarshal(out, &got); err != nil {
					errs <- err
					return
				}
				if got != tc.want {
					// A wrong answer here is the failure mode that matters:
					// it means one evaluation read another's input.
					errs <- &wrongAnswer{expr: tc.expr, got: got, want: tc.want}
					return
				}
			}
		}()
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent evaluation failed: %v", err)
	}
}

type wrongAnswer struct {
	expr string
	got  float64
	want float64
}

func (w *wrongAnswer) Error() string {
	return w.expr + " produced a value from another goroutine's input"
}

// TestLambdaCallingABuiltinRacesATopLevelCallOfIt is the shape the first two
// tests miss. Neither $sum/$count/arithmetic at top level, nor one compiled
// expression evaluated twice, exercises applyFunction's write onto a
// built-in's shared *Function value (token, position -- v206/jsonata.go) --
// that write only happens through function APPLICATION, and a top-level
// `$count(items)` never applies $count as a value, it calls it directly.
// Wrapping the same call in a lambda does apply it, and that is enough to
// race a concurrent top-level call of the SAME builtin, from a completely
// separate instance and expression -- unlocked, this reproduces reliably
// under -race.
//
// Locked through TWO SEPARATE exprCaches -- standing in for two different
// packages (this one, and jsonmapper) evaluating concurrently, each holding
// only its own cache's compile lock -- it must not race, because both now
// take jsonmapper.Evaluating for the Evaluate call itself. This is the
// exact scenario that lock exists to close: a lock scoped to one cache's
// compile step is not the same as a lock scoped to evaluation.
func TestLambdaCallingABuiltinRacesATopLevelCallOfIt(t *testing.T) {
	cacheA, err := newExprCache()
	if err != nil {
		t.Fatalf("newExprCache: %v", err)
	}
	cacheB, err := newExprCache()
	if err != nil {
		t.Fatalf("newExprCache: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 16*300)

	for g := 0; g < 16; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				var got any
				var err error
				if g%2 == 0 {
					got, err = cacheA.evaluate(`( $f := function($x){ $count($x) }; $f(items) )`,
						map[string]any{"items": []any{1, 2, 3, 4, 5}})
				} else {
					got, err = cacheB.evaluate(`$count(c)`, map[string]any{"c": []any{1, 2}})
				}
				if err != nil {
					errs <- err
					return
				}
				want := float64(5)
				if g%2 != 0 {
					want = 2
				}
				if number, ok := got.(float64); !ok || number != want {
					errs <- fmt.Errorf("got %v, want %v -- the shared lock did not cover this call shape", got, want)
					return
				}
			}
		}()
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("%v", err)
	}
}

// The other half of the question: ONE compiled expression evaluated
// concurrently. The library binds the input onto the expression's own
// environment (`execEnv.bind("$", input)` when there are no bindings) and
// writes through the expression's timestamp pointer, so this IS a race.
//
// It is what jsonmapper.Evaluating exists to prevent, and it is
// per-expression -- which is why one process-wide lock, not one per package,
// is the right width.
func TestOneCompiledExpressionNeedsALock(t *testing.T) {
	cache, err := newExprCache()
	if err != nil {
		t.Fatalf("newExprCache: %v", err)
	}

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				// Through the package's own evaluate, which takes the lock.
				got, err := cache.evaluate("a + b", map[string]any{"a": n, "b": 1})
				if err != nil {
					t.Errorf("evaluate: %v", err)
					return
				}
				if number, ok := got.(float64); !ok || number != float64(n+1) {
					t.Errorf("a + b = %v, want %d -- another goroutine's input leaked in", got, n+1)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

// ---- input resolution and rendering (formerly inputs_test.go) ----

// resolveInputsFixture wraps a bare inputs map in the minimum Spec that
// ResolveInputs needs. The zone is part of the Spec so a pipeline's schedule
// and its date window cannot drift apart; these tests are about resolution
// itself, so they state one and move on.
func resolveInputsFixture(inputs map[string]Input, lookup func(string) (string, bool)) (map[string]string, error) {
	return ResolveInputs(specWith(inputs), lookup)
}

func resolveInputsAtFixture(inputs map[string]Input, lookup func(string) (string, bool), now time.Time) (map[string]string, error) {
	return resolveInputsAt(specWith(inputs), lookup, now)
}

func specWith(inputs map[string]Input) Spec {
	return Spec{Inputs: inputs, Schedule: Schedule{Cron: "0 0 * * *", UTCOffset: "+05:30"}}
}

// lookupFrom makes an os.LookupEnv-shaped function out of a map, so a test
// states the environment it means instead of mutating the process's.
func lookupFrom(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	}
}

func TestResolveInputsEnvBeatsDefault(t *testing.T) {
	inputs := map[string]Input{
		"baseUrl": {Env: "MANDI_API_URI", Default: "http://default:8080"},
	}

	got, err := resolveInputsFixture(inputs, lookupFrom(map[string]string{
		"MANDI_API_URI": "http://set-by-env:9090",
	}))
	if err != nil {
		t.Fatalf("resolveInputs: %v", err)
	}
	if want := "http://set-by-env:9090"; got["baseUrl"] != want {
		t.Errorf("baseUrl = %q, want %q -- a set env var must beat the default", got["baseUrl"], want)
	}
}

// An env var that is present but empty is the shape a misconfigured
// deployment takes (`MANDI_API_URI=` in a compose file), and treating it as a
// deliberate empty override would point the run at nothing at all. It falls
// back to the default instead.
func TestResolveInputsDefaultWhenEnvAbsentOrEmpty(t *testing.T) {
	inputs := map[string]Input{
		"baseUrl": {Env: "MANDI_API_URI", Default: "http://default:8080"},
	}

	for name, env := range map[string]map[string]string{
		"absent": {},
		"empty":  {"MANDI_API_URI": ""},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := resolveInputsFixture(inputs, lookupFrom(env))
			if err != nil {
				t.Fatalf("resolveInputs: %v", err)
			}
			if want := "http://default:8080"; got["baseUrl"] != want {
				t.Errorf("baseUrl = %q, want %q", got["baseUrl"], want)
			}
		})
	}
}

// Default is `interface{}` because YAML scalars are not all strings. A %s on
// an int yields "%!s(int=256)", which is not a value any upstream understands
// and is not obviously wrong when read in a log line.
func TestResolveInputsRendersNonStringDefaults(t *testing.T) {
	inputs := map[string]Input{
		"budget":  {Default: 256},
		"enabled": {Default: true},
		"ratio":   {Default: 1.5},
	}

	got, err := resolveInputsFixture(inputs, lookupFrom(nil))
	if err != nil {
		t.Fatalf("resolveInputs: %v", err)
	}
	for key, want := range map[string]string{
		"budget":  "256",
		"enabled": "true",
		"ratio":   "1.5",
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
}

// states declares `default: []` with the comment "empty means every state the
// upstream reports". Rendered as "[]" that becomes a state literally named
// "[]"; it has to come out empty for the caller's "no states named" branch to
// be reachable.
func TestResolveInputsRendersListDefaults(t *testing.T) {
	inputs := map[string]Input{
		"none": {Type: "list", Default: []interface{}{}},
		"some": {Type: "list", Default: []interface{}{"Kerala", "Punjab"}},
	}

	got, err := resolveInputsFixture(inputs, lookupFrom(nil))
	if err != nil {
		t.Fatalf("resolveInputs: %v", err)
	}
	if got["none"] != "" {
		t.Errorf("none = %q, want %q", got["none"], "")
	}
	if want := "Kerala,Punjab"; got["some"] != want {
		t.Errorf("some = %q, want %q", got["some"], want)
	}
}

// An input with neither an env value nor a default is NOT an error here. The
// caller knows which inputs its run actually needs, so it can fail with
// "publishUrl is not set (CATALOG_PUBLISH_URL)" -- a message naming the thing
// that is missing -- where this function could only say "an input".
func TestResolveInputsUnsetIsEmptyNotAnError(t *testing.T) {
	inputs := map[string]Input{
		"publishUrl":  {Env: "CATALOG_PUBLISH_URL"},
		"tokenSecret": {Env: "MANDI_TOKEN_SECRET", Secret: true},
	}

	got, err := resolveInputsFixture(inputs, lookupFrom(nil))
	if err != nil {
		t.Fatalf("resolveInputs returned an error for unset inputs: %v -- "+
			"requiredness is the caller's call, not this function's", err)
	}
	for _, key := range []string{"publishUrl", "tokenSecret"} {
		value, ok := got[key]
		if !ok {
			t.Errorf("%q missing from the result; an unset input must still be present", key)
		}
		if value != "" {
			t.Errorf("%s = %q, want %q", key, value, "")
		}
	}
}

// Callers index the result by name. A key dropped because it resolved to
// nothing turns a configuration mistake into a lookup miss somewhere else.
func TestResolveInputsReturnsEveryKey(t *testing.T) {
	inputs := map[string]Input{
		"withEnv":     {Env: "SET", Default: "d"},
		"withDefault": {Default: "d"},
		"withNeither": {},
	}

	got, err := resolveInputsFixture(inputs, lookupFrom(map[string]string{"SET": "v"}))
	if err != nil {
		t.Fatalf("resolveInputs: %v", err)
	}
	if len(got) != len(inputs) {
		t.Errorf("got %d resolved inputs, want %d: %v", len(got), len(inputs), got)
	}
	for key := range inputs {
		if _, ok := got[key]; !ok {
			t.Errorf("%q missing from the result", key)
		}
	}
}

// TestResolveInputsEnforcesEnum covers the one constraint the file states that
// resolution can check on its own. An unenforced enum is worse than none:
// withoutGeometry reaching the catalog build as anything but publish or skip
// takes the "publish" branch by default, so a typo would ship geometry-less
// markets an operator believed were being skipped.
func TestResolveInputsEnforcesEnum(t *testing.T) {
	inputs := map[string]Input{
		"withoutGeometry": {Env: "WITHOUT_GEOMETRY", Enum: []string{"publish", "skip"}, Default: "publish"},
	}

	t.Run("allowed value passes", func(t *testing.T) {
		got, err := resolveInputsFixture(inputs, func(string) (string, bool) { return "skip", true })
		if err != nil {
			t.Fatalf("resolveInputs: %v", err)
		}
		if got["withoutGeometry"] != "skip" {
			t.Errorf("withoutGeometry = %q, want skip", got["withoutGeometry"])
		}
	})

	t.Run("value outside the enum is refused", func(t *testing.T) {
		_, err := resolveInputsFixture(inputs, func(string) (string, bool) { return "publsh", true })
		if err == nil {
			t.Fatal("a value outside the declared enum resolved without error")
		}
		for _, want := range []string{"withoutGeometry", "publsh", "publish", "skip"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		}
	})

	t.Run("unresolved value is not an enum violation", func(t *testing.T) {
		noDefault := map[string]Input{
			"withoutGeometry": {Env: "WITHOUT_GEOMETRY", Enum: []string{"publish", "skip"}},
		}
		got, err := resolveInputsFixture(noDefault, func(string) (string, bool) { return "", false })
		if err != nil {
			t.Fatalf("an unresolved input was treated as an enum violation: %v", err)
		}
		if got["withoutGeometry"] != "" {
			t.Errorf("withoutGeometry = %q, want empty", got["withoutGeometry"])
		}
	})
}

// TestResolveInputsResolvesDateDefaults covers an input the file declares as a
// date and this package previously handed on as the literal word "today".
//
// fromDate/toDate are declared `type: date, format: dd-MM-yyyy, default:
// today`. The resolved value goes into market-commodity.yaml's query and
// reaches the upstream as a date, so "today" is not a harmless placeholder --
// it is a malformed request the upstream answers with nothing, which this
// pipeline would then read as "no markets traded".
func TestResolveInputsResolvesDateDefaults(t *testing.T) {
	// A fixed clock, so the assertion is an exact string rather than a
	// restatement of the formatting code.
	at := time.Date(2026, time.September, 7, 3, 30, 0, 0, time.UTC)

	inputs := map[string]Input{
		"fromDate": {Type: "date", Format: "dd-MM-yyyy", Default: "today"},
	}
	got, err := resolveInputsAtFixture(inputs, func(string) (string, bool) { return "", false }, at)
	if err != nil {
		t.Fatalf("resolveInputsAt: %v", err)
	}
	// 03:30 UTC on the 7th is 09:00 IST on the 7th -- same date either way,
	// chosen so this asserts the format rather than the timezone.
	if got["fromDate"] != "07-09-2026" {
		t.Errorf("fromDate = %q, want %q (dd-MM-yyyy, not Go's reference layout and not the word today)",
			got["fromDate"], "07-09-2026")
	}
}

// TestResolveInputsDateUsesPipelineTimezone pins the boundary case the
// timezone actually decides. The pipeline is scheduled at 00:00 Asia/Kolkata,
// so a run fires when UTC is still on the previous day; resolving "today" in
// UTC would ask the upstream for yesterday, every single night.
func TestResolveInputsDateUsesPipelineTimezone(t *testing.T) {
	// 20:00 UTC on the 6th is 01:30 IST on the 7th -- just after a midnight run.
	at := time.Date(2026, time.September, 6, 20, 0, 0, 0, time.UTC)

	inputs := map[string]Input{
		"fromDate": {Type: "date", Format: "dd-MM-yyyy", Default: "today"},
	}
	got, err := resolveInputsAtFixture(inputs, func(string) (string, bool) { return "", false }, at)
	if err != nil {
		t.Fatalf("resolveInputsAt: %v", err)
	}
	if got["fromDate"] != "07-09-2026" {
		t.Errorf("fromDate = %q, want %q -- resolved in UTC instead of the pipeline's timezone, "+
			"which asks the upstream for yesterday on every midnight run", got["fromDate"], "07-09-2026")
	}
}

// TestResolveInputsRejectsUnknownTypeAndFormat: an unrecognised type or an
// untranslatable format must refuse rather than pass a value through that
// nothing downstream can use. Silent passthrough is what made "today" reach
// the upstream in the first place.
func TestResolveInputsRejectsUnknownTypeAndFormat(t *testing.T) {
	for name, inputs := range map[string]map[string]Input{
		"unknown type": {
			"odd": {Type: "duration", Default: "5m"},
		},
		"untranslatable date format": {
			"fromDate": {Type: "date", Format: "RFC3339", Default: "today"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := resolveInputsFixture(inputs, func(string) (string, bool) { return "", false }); err == nil {
				t.Error("resolved without error")
			}
		})
	}
}

// TestRedactedInputsHidesSecrets: the resolved map carries credentials, so
// anything that prints it must print the redacted copy instead.
func TestRedactedInputsHidesSecrets(t *testing.T) {
	inputs := map[string]Input{
		"tokenSecret": {Env: "MANDI_TOKEN_SECRET", Secret: true},
		"baseUrl":     {Env: "MANDI_API_URI"},
	}
	resolved, err := resolveInputsFixture(inputs, lookupFrom(map[string]string{
		"MANDI_TOKEN_SECRET": "hunter2",
		"MANDI_API_URI":      "http://upstream.test",
	}))
	if err != nil {
		t.Fatalf("resolveInputs: %v", err)
	}
	if resolved["tokenSecret"] != "hunter2" {
		t.Fatalf("the raw map must still carry the real secret, got %q", resolved["tokenSecret"])
	}

	safe := RedactedInputs(inputs, resolved)
	if strings.Contains(fmt.Sprint(safe), "hunter2") {
		t.Errorf("redactedInputs leaked the secret: %v", safe)
	}
	if safe["baseUrl"] != "http://upstream.test" {
		t.Errorf("redactedInputs altered a non-secret: %q", safe["baseUrl"])
	}
}

// yesterday is resolved in the schedule's zone from the run's clock: a
// midnight IST firing asks for the day that just closed, not UTC's, and not
// the day that is seconds old.
// "N days ago" is a rolling window edge, resolved in the schedule zone the
// same way yesterday is.
func TestDaysAgoResolvesInTheScheduleZone(t *testing.T) {
	spec := Spec{
		Schedule: Schedule{UTCOffset: "+05:30"},
		Inputs: map[string]Input{
			"weekAgo": {Type: "date", Format: "dd-MM-yyyy", Default: "7 days ago"},
			"dayAgo":  {Type: "date", Format: "dd-MM-yyyy", Default: "1 day ago"},
		},
	}
	// 00:05 IST on the 21st; seven days before the 21st is the 14th.
	now := time.Date(2026, 9, 20, 18, 35, 0, 0, time.UTC)
	resolved, err := resolveInputsAt(spec, func(string) (string, bool) { return "", false }, now)
	if err != nil {
		t.Fatalf("resolveInputsAt: %v", err)
	}
	if got := resolved["weekAgo"]; got != "14-09-2026" {
		t.Errorf("weekAgo = %q, want 14-09-2026", got)
	}
	if got := resolved["dayAgo"]; got != "20-09-2026" {
		t.Errorf("dayAgo = %q, want 20-09-2026 (the same day yesterday names)", got)
	}
}

// An env override may use the same relative form.
func TestDaysAgoIsAcceptedFromTheEnvironment(t *testing.T) {
	spec := Spec{
		Schedule: Schedule{UTCOffset: "+05:30"},
		Inputs:   map[string]Input{"from": {Env: "FROM", Type: "date", Format: "dd-MM-yyyy", Default: "yesterday"}},
	}
	now := time.Date(2026, 9, 20, 18, 35, 0, 0, time.UTC)
	lookup := func(name string) (string, bool) {
		if name == "FROM" {
			return "30 days ago", true
		}
		return "", false
	}
	resolved, err := resolveInputsAt(spec, lookup, now)
	if err != nil {
		t.Fatalf("resolveInputsAt: %v", err)
	}
	if got := resolved["from"]; got != "22-08-2026" {
		t.Fatalf("from = %q, want 22-08-2026", got)
	}
}

func TestYesterdayResolvesInTheScheduleZone(t *testing.T) {
	spec := Spec{
		Schedule: Schedule{UTCOffset: "+05:30"},
		Inputs:   map[string]Input{"fromDate": {Type: "date", Format: "dd-MM-yyyy", Default: "yesterday"}},
	}
	// 00:05 IST on the 21st is 18:35 UTC on the 20th; yesterday in IST is the 20th.
	now := time.Date(2026, 9, 20, 18, 35, 0, 0, time.UTC)
	resolved, err := resolveInputsAt(spec, func(string) (string, bool) { return "", false }, now)
	if err != nil {
		t.Fatalf("resolveInputsAt: %v", err)
	}
	if got := resolved["fromDate"]; got != "20-09-2026" {
		t.Fatalf("fromDate = %q, want 20-09-2026", got)
	}
}
