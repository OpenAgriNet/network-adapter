// Package util holds what the capability machinery needs but does not own: the
// retry budget, the endpoint and body of a request, and every constant.
//
// It exists because these have no receiver on common's types, so they can sit
// in their own package -- and because common imports it, the constants live
// here too. Named plainly, since the contents are a mix rather than one
// subject: the retry budget, the query rendering, the auth scheme names.
//
// Not a home for anything that will fit elsewhere. A helper belongs beside the
// code that uses it unless a receiver forbids it.
package util

import (
	"context"
	"errors"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

// Budget resolves how long one attempt may take and how many retries follow,
// applying the registry's values within this deployment's ceilings.
//
// Separate from call so both bounds can be asserted without a server that
// sleeps for the timeout being tested.
//
// retryMax counts retries, not attempts: the call is always made once, and an
// absent retryMax means the same as an explicit 0.
func Budget(call model.ActionPlan) (time.Duration, int) {
	timeout := DefaultTimeout
	if call.TimeoutMs > 0 {
		timeout = time.Duration(call.TimeoutMs) * time.Millisecond
	}
	if timeout > MaxTimeout {
		timeout = MaxTimeout
	}

	retries := DefaultRetryMax
	if call.RetryMax > 0 {
		retries = call.RetryMax
	}
	if retries > MaxRetryMax {
		retries = MaxRetryMax
	}
	return timeout, retries
}

// permanentErr marks a failure no retry can fix. Kept unexported and detected
// with errors.As, so a caller of this package sees only the underlying error.
type permanentErr struct{ error }

func (p permanentErr) Unwrap() error { return p.error }

// DoNotRetry marks err as not worth repeating.
func DoNotRetry(err error) error { return permanentErr{err} }

// IsPermanent reports whether err is one no further attempt would change.
func IsPermanent(err error) bool {
	var permanent permanentErr
	return errors.As(err, &permanent)
}

// Backoff is how long to wait before the next attempt.
//
// Exponential from a short base and capped: a briefly busy provider is worth
// waiting out, anything longer is a timeout's job. With no wait, retryMax 5
// spends its Budget in a couple of milliseconds -- the same failure six times.
func Backoff(attempt int) time.Duration {
	if attempt <= 1 {
		return RetryBackoffBase
	}
	// Doubled in a loop that stops at the ceiling, not shifted then clamped.
	// `base << (attempt-1)` overflows int64 around shift 38 and the wrapped
	// value is NEGATIVE, so it passes the ceiling check and sleeps for no time
	// -- the retry loop then spins as fast as the provider can refuse.
	wait := RetryBackoffBase
	for i := 1; i < attempt && wait < RetryBackoffMax; i++ {
		wait *= 2
	}
	if wait > RetryBackoffMax {
		return RetryBackoffMax
	}
	return wait
}

// Sleep waits, or reports that the context ended first.
func Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
