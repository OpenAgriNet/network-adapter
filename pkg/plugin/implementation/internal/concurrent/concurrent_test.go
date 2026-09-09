package concurrent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunReturnsResultsInOrderNotArrivalOrder(t *testing.T) {
	t.Parallel()

	// index 0 is the slowest, so an arrival-ordered result would put it
	// last -- this proves the result is indexed, not appended.
	delays := []time.Duration{30 * time.Millisecond, 10 * time.Millisecond, 0}
	results, err := Run(context.Background(), len(delays), len(delays),
		func(ctx context.Context, i int) (int, error) {
			time.Sleep(delays[i])
			return i, nil
		})
	if err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}
	for i, got := range results {
		if got != i {
			t.Errorf("results[%d] = %d, want %d", i, got, i)
		}
	}
}

func TestRunIsSequentialWhenLimitIsOne(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var inFlight, peak int
	_, err := Run(context.Background(), 4, 1, func(ctx context.Context, i int) (struct{}, error) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()

		time.Sleep(10 * time.Millisecond)

		mu.Lock()
		inFlight--
		mu.Unlock()
		return struct{}{}, nil
	})
	if err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}
	if peak != 1 {
		t.Errorf("peak concurrency = %d, want 1", peak)
	}
}

func TestRunHonoursALimitAboveOne(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var inFlight, peak int
	_, err := Run(context.Background(), 6, 2, func(ctx context.Context, i int) (struct{}, error) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()

		time.Sleep(15 * time.Millisecond)

		mu.Lock()
		inFlight--
		mu.Unlock()
		return struct{}{}, nil
	})
	if err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}
	if peak != 2 {
		t.Errorf("peak concurrency = %d, want the configured 2", peak)
	}
}

func TestRunReturnsTheCallsOwnError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("boom")
	_, err := Run(context.Background(), 3, 3, func(ctx context.Context, i int) (int, error) {
		if i == 1 {
			return 0, fmt.Errorf("call %d failed: %w", i, sentinel)
		}
		return i, nil
	})
	if err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want it to wrap the call's own error", err)
	}
}

func TestRunSkipsCallsNotYetIssuedAfterAFailureWhenSequential(t *testing.T) {
	t.Parallel()

	var called int32
	_, err := Run(context.Background(), 3, 1, func(ctx context.Context, i int) (struct{}, error) {
		atomic.AddInt32(&called, 1)
		if i == 0 {
			return struct{}{}, errors.New("first call fails")
		}
		return struct{}{}, nil
	})
	if err == nil {
		t.Fatal("Run() returned no error, want the first call's failure")
	}
	// Sequential (limit 1): index 0 fails and cancels the shared context
	// before index 1's call is ever issued, so only index 0 was called.
	if got := atomic.LoadInt32(&called); got != 1 {
		t.Errorf("work was called %d times, want 1 -- calls not yet issued "+
			"when an earlier one fails must be skipped, not made and discarded", got)
	}
}

func TestRunOfZeroReturnsNoResultsAndNoError(t *testing.T) {
	t.Parallel()

	results, err := Run(context.Background(), 0, 4, func(ctx context.Context, i int) (int, error) {
		t.Error("work was called for n == 0")
		return 0, nil
	})
	if err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %v, want empty", results)
	}
}

func TestRunOfOneRunsExactlyOnce(t *testing.T) {
	t.Parallel()

	var called int32
	results, err := Run(context.Background(), 1, 1, func(ctx context.Context, i int) (string, error) {
		atomic.AddInt32(&called, 1)
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}
	if got := atomic.LoadInt32(&called); got != 1 {
		t.Errorf("work was called %d times, want 1", got)
	}
	if len(results) != 1 || results[0] != "ok" {
		t.Errorf("results = %v, want [\"ok\"]", results)
	}
}

func TestRunCancelsTheContextItHandsToInFlightWork(t *testing.T) {
	t.Parallel()

	var slowCtxDone int32
	// inFlight is closed by index 1 once it is genuinely running, and index 0
	// waits for that before failing. Without the handshake this test is a
	// race: index 1's call may not have started when index 0 fails, and Run
	// then SKIPS it rather than cancelling it -- correct behaviour, and what
	// TestRunSkipsCallsNotYetIssuedAfterAFailureWhenSequential is for, but
	// not what this test is about.
	inFlight := make(chan struct{})
	_, err := Run(context.Background(), 2, 2, func(ctx context.Context, i int) (struct{}, error) {
		if i == 0 {
			<-inFlight
			return struct{}{}, errors.New("fails once the other call is in flight")
		}
		close(inFlight)
		// In flight when index 0 fails: wait for our own ctx to be cancelled,
		// or give up after a bound that would mean it never was. A bound
		// rather than a bare receive so a broken Run fails this test instead
		// of hanging it.
		select {
		case <-ctx.Done():
			atomic.AddInt32(&slowCtxDone, 1)
		case <-time.After(2 * time.Second):
		}
		return struct{}{}, nil
	})
	if err == nil {
		t.Fatal("Run() returned no error, want index 0's failure")
	}
	if atomic.LoadInt32(&slowCtxDone) != 1 {
		t.Error("the in-flight call's context was not cancelled after the other call failed")
	}
}

// --- Map --------------------------------------------------------------------

// Map hands each value to its own call and returns the answers in the order
// the values were given, whatever order the calls finished in.
func TestMapCallsOncePerValueInOrder(t *testing.T) {
	t.Parallel()

	values := []string{"kvk", "warehouse", "soil_lab"}
	results, err := Map(context.Background(), values, 3, func(ctx context.Context, value string) (string, error) {
		// The first value sleeps longest, so arrival order is the reverse of
		// the order asked for. A caller relying on results[i] meaning "the
		// answer for values[i]" has to hold regardless.
		if value == "kvk" {
			time.Sleep(40 * time.Millisecond)
		}
		return "answered:" + value, nil
	})
	if err != nil {
		t.Fatalf("Map() returned an unexpected error: %v", err)
	}
	want := []string{"answered:kvk", "answered:warehouse", "answered:soil_lab"}
	if len(results) != len(want) {
		t.Fatalf("results = %v, want %v", results, want)
	}
	for index := range want {
		if results[index] != want[index] {
			t.Errorf("results[%d] = %q, want %q -- value order, not arrival order",
				index, results[index], want[index])
		}
	}
}

// An empty list makes no calls and is not an error: a caller deciding that an
// empty list is a bad request says so itself, in its own words.
func TestMapOfNoValuesMakesNoCalls(t *testing.T) {
	t.Parallel()

	results, err := Map(context.Background(), nil, 2, func(ctx context.Context, value string) (int, error) {
		t.Error("a call was made for an empty value list")
		return 0, nil
	})
	if err != nil {
		t.Fatalf("Map() returned an unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %v, want empty", results)
	}
}

// One value's failure fails the whole call, and the value's own error is what
// comes back -- the caller named which value it was and why.
func TestMapFailsWhenOneValueFails(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("warehouse is unreachable")
	_, err := Map(context.Background(), []string{"kvk", "warehouse"}, 1,
		func(ctx context.Context, value string) (string, error) {
			if value == "warehouse" {
				return "", sentinel
			}
			return "ok", nil
		})
	if !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want the failing value's own error", err)
	}
}

// --- Bound ------------------------------------------------------------------

// Bound is the arithmetic every caller of Run and Map needs: a configured
// limit, a fallback when it is unset, a ceiling when it is over. Zero must
// become the fallback and not reach errgroup, where it means UNBOUNDED.
func TestBoundDefaultsAndClamps(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name                                string
		configured, fallback, ceiling, want int
	}{
		{"unset takes the fallback", 0, 1, 8, 1},
		{"negative takes the fallback", -4, 2, 8, 2},
		{"within range is honoured", 4, 1, 8, 4},
		{"exactly the ceiling is honoured", 8, 1, 8, 8},
		{"over the ceiling is clamped", 64, 1, 8, 8},
		{"a fallback over the ceiling is clamped too", 0, 16, 8, 8},
	} {
		if got := Bound(tc.configured, tc.fallback, tc.ceiling); got != tc.want {
			t.Errorf("%s: Bound(%d, %d, %d) = %d, want %d",
				tc.name, tc.configured, tc.fallback, tc.ceiling, got, tc.want)
		}
	}
}
