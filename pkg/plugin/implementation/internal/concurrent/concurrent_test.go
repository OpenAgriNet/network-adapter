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
	_, err := Run(context.Background(), 2, 2, func(ctx context.Context, i int) (struct{}, error) {
		if i == 0 {
			return struct{}{}, errors.New("fails immediately")
		}
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
