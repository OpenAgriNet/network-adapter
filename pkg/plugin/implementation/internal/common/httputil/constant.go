// Constants for the call and its retry budget.
//
// Here rather than in common's constant.go because the code that reads them
// is here, and common imports this package -- so a shared file would be an
// import cycle.
package httputil

import (
	"time"
)

// Defaults applied when the registry or the operator leaves a setting out.
const (
	// Zero retries is deliberate: a retry on a non-idempotent action is a
	// second booking, so a provider is retried only where the operator said so.
	DefaultTimeout  = 15 * time.Second
	DefaultRetryMax = 0

	// A ceiling on what a registry row may ask for, because the row is DATA and
	// an attempt holds a goroutine and the inbound connection for its whole
	// timeout. retryMax 1000 with timeoutMs 60000 would pin both for about
	// seventeen hours.
	//
	// Clamped rather than refused: a row that overreaches is a mistake, and
	// serving it with a sane budget beats failing the capability outright.
	MaxTimeout  = 30 * time.Second
	MaxRetryMax = 5
)

// How long this step waits between attempts.
const (
	// The first wait, doubling up to the max. Short: an operator setting 5
	// retries asked to ride out a brief outage, not for seconds of latency.
	RetryBackoffBase = 50 * time.Millisecond
	RetryBackoffMax  = 800 * time.Millisecond
)

// How much of a failed response is quoted: enough for the provider's message,
// not a page of HTML in a log line.
const explainLimit = 300
