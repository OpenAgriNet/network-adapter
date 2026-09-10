// Package concurrent is one generic engine: run a known number of
// independent calls, bounded, with the first failure cancelling the rest.
//
// It knows nothing about Beckn, upstream providers, or any specific
// capability. Every policy question -- how many is too many, what identity
// each call carries, what a failure should say -- belongs to whoever calls
// this, not to this package: see
// pkg/plugin/implementation/AgricultureFacility/search.go for the one caller
// that exists today.
//
// pkg/plugin/implementation/catalogpublisher's own runConcurrent looks
// similar but is NOT this: it collects every call's error rather than
// cancelling on the first one, because its caller needs to inspect all of
// them to decide whether any is worth retrying. That is a different
// concurrency shape, kept separate rather than forced into one function
// with a mode flag.
package concurrent

import (
	"context"
	"fmt"

	"golang.org/x/sync/errgroup"
)

// Run calls work(ctx, i) for every i in [0, n), at most limit at a time, and
// returns each call's result at its own index -- in the order Run was asked
// for them, not the order they finished, so a caller can rely on results[i]
// meaning "the i-th thing I asked for" regardless of which one answered
// first.
//
// The first error any call returns cancels the ctx handed to every other
// call, in flight or not yet started: a call not yet issued when an earlier
// one already failed is skipped rather than made and discarded, and an
// in-flight call that itself respects ctx cancellation (as an
// http.NewRequestWithContext-built request does) is aborted rather than run
// to completion for an answer nobody will use. Run itself returns that first
// error, wrapped by nothing -- whatever work returned is what the caller
// gets, so the caller's own wrapping (naming which of the n things failed,
// what identity it carried) survives unchanged. What each call's own input or
// identity looks like is entirely work's business -- Run passes only an
// index, so it never has to know or care what a caller maps that index onto.
//
// A cancelled PARENT ctx -- a disconnected client, a handler that ran out of
// time -- is reported as an error too, never as results. The ctx errgroup
// hands out is cancelled by both causes, the first work error and the parent
// going away, and a Run that answered a cancelled parent with (zero values,
// nil) would tell its caller every call succeeded: the silent-partial-answer
// failure its fail-fast contract exists to prevent.
//
// n == 0 calls work zero times and returns (nil, nil). limit is passed
// straight to errgroup.Group.SetLimit -- see its own doc for what a
// non-positive value means.
func Run[T any](ctx context.Context, n, limit int, work func(ctx context.Context, index int) (T, error)) ([]T, error) {
	if n == 0 {
		return nil, nil
	}

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(limit)

	results := make([]T, n)
	for index := range n {
		group.Go(func() error {
			// Reported rather than swallowed. context.Cause names which
			// cancellation this is: the first work error when a sibling
			// failed -- which is what group.Wait already returns, so saying it
			// again changes nothing -- and the PARENT's own reason when the
			// parent went away, which nothing else here would report at all.
			if groupCtx.Err() != nil {
				return context.Cause(groupCtx)
			}
			result, err := work(groupCtx, index)
			if err != nil {
				return fmt.Errorf("%w", err)
			}
			results[index] = result
			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return nil, err
	}
	return results, nil
}

// Map is Run over a list of VALUES rather than a count: one call per value,
// results in the order the values were given.
//
// The same bounded, ordered, fail-fast guarantees as Run -- it is Run, with
// the index-to-value step done here instead of in every caller. Prefer it
// whenever the work is "one call per thing in this list".
//
// values is []V and results are []T, so a caller keeps whatever types it
// already has; nothing here inspects either.
//
// What Map does NOT decide, on purpose: how many values are too many, and
// what to tell a caller who asked for too many. A ceiling is a fact about
// whatever is being called and its refusal is that domain's error to write,
// so it belongs to the package that knows the domain. Use Bound below for the
// other half of the arithmetic, defaulting and clamping a configured
// concurrency.
func Map[V, T any](ctx context.Context, values []V, limit int,
	call func(ctx context.Context, value V) (T, error)) ([]T, error) {

	return Run(ctx, len(values), limit, func(ctx context.Context, index int) (T, error) {
		return call(ctx, values[index])
	})
}

// Bound resolves a configured concurrency: fallback when it is unset or
// non-positive, ceiling when it is over.
//
// Here rather than in each caller because every caller of Run or Map needs
// the same three lines, and getting them wrong is quiet: a zero limit means
// UNBOUNDED in errgroup, so a deployment that leaves the setting out would get
// every call at once from a mistake that reads like a default.
func Bound(configured, fallback, ceiling int) int {
	limit := configured
	if limit <= 0 {
		limit = fallback
	}
	if limit > ceiling {
		limit = ceiling
	}
	return limit
}
