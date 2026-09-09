// Constants only. authFields is not here: it is a var, since Go has no constant
// maps, and it sits beside the parser in auth.go that reads it.
package common

import (
	"time"
)

// Defaults applied when the registry or the operator leaves a setting out.
const (
	// Zero retries is deliberate: a retry on a non-idempotent action is a
	// second booking, so a provider is retried only where the operator said so.
	DefaultTimeout  = 15 * time.Second
	DefaultRetryMax = 0
	// The response is mapped in memory, so an unbounded one is an unbounded
	// allocation.
	DefaultMaxResponseBytes = 4 << 20 // 4 MiB

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

// redactedMarker stands in for a credential in anything logged or returned.
const redactedMarker = "REDACTED"

// Auth schemes a step can present. Credentials are never in config or in the
// registry: config names the environment variable to read.
const (
	AuthSchemeNone   = "none"
	AuthSchemeBasic  = "basic"
	AuthSchemeHeader = "header"
	// The credential goes in the query string, which some upstreams require.
	// The least safe scheme -- proxies log query strings and transport errors
	// quote them -- so the value is redacted. See redact.
	AuthSchemeQuery = "query"

	// A client id and secret are exchanged for a short-lived bearer token. For
	// an upstream where a static token is not an option: the live one this was
	// written against expires every ten hours.
	//
	// Only client_credentials. That grant has no user to redirect and no
	// refresh token, so the other flows would be dead code.
	AuthSchemeOAuth2 = "oauth2"
)

// codeUpstreamUnavailable: the provider could not be reached or failed. Not
// this adapter's fault and not the caller's.
const codeUpstreamUnavailable = "NET_DOWNSTREAM_UNAVAILABLE"

// How early an oauth2 token stops being trusted. Must exceed the round trip to
// the provider, so a request that passed the expiry check cannot arrive after
// the token died. Costs one extra exchange per lifetime.
const tokenRefreshSkew = 60 * time.Second

// How much of a failed response is quoted: enough for the provider's message,
// not a page of HTML in a log line.
const explainLimit = 300
