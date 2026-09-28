package pipeline

// expr_race_test.go pins a load-bearing fact about the JSONata library:
// whether two INDEPENDENT evaluators, each with its own instance and its own
// compiled expressions, share mutable state.
//
// The answer decides how wide the lock in expr.go has to be. If the library
// keeps evaluation state in package-level variables, then this package's lock
// and jsonmapper's lock are two locks over one resource and neither is
// sufficient -- the publish sweep runs in the adapter process beside
// reqmapper on live traffic, so the two would evaluate concurrently.
//
// Run it with -race. Without -race it proves almost nothing.

import (
	"encoding/json"
	"sync"
	"testing"

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

// The other half of the question: ONE compiled expression evaluated
// concurrently. The library binds the input onto the expression's own
// environment (`execEnv.bind("$", input)` when there are no bindings) and
// writes through the expression's timestamp pointer, so this IS a race.
//
// It is what each package's lock exists to prevent, and it is per-expression
// -- which is why one lock per package is the right width.
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
