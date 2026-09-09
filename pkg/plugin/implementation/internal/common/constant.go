// Constants only: the budgets a registry row is clamped to, the retry backoff,
// the auth schemes, and the markers this package puts in text it returns.
//
// authFields is deliberately NOT here. It is a var -- Go has no constant maps --
// and it belongs beside the parser in auth.go that reads it.
package common

import (
	"time"
)

// Defaults applied when the registry or the operator leaves a setting out.
const (
	// DefaultTimeout and DefaultRetryMax are the registry contract's defaults
	// for an action that leaves timeoutMs or retryMax out. Zero retries is
	// deliberate: a provider that failed is retried only where the operator
	// said so, because a retry on a non-idempotent action is a second booking.
	DefaultTimeout  = 15 * time.Second
	DefaultRetryMax = 0
	// DefaultMaxResponseBytes caps what is read from the provider. The response
	// is mapped in memory, so an unbounded one is an unbounded allocation.
	DefaultMaxResponseBytes = 4 << 20 // 4 MiB

	// MaxTimeout and MaxRetryMax bound what a registry row may ask for.
	//
	// Both come from DATA, not from this deployment's config, and neither is
	// cheap: an attempt holds a goroutine and the inbound connection for its
	// whole timeout, and http.Server's write timeout does not cancel the
	// request context. So a row reading retryMax 1000, timeoutMs 60000 pins
	// both for roughly seventeen hours, and a handful of such requests is the
	// adapter. The registry is trusted to say where a provider is; it is not a
	// reason to let one row decide how long this process is busy.
	//
	// Clamped rather than refused. A row that overreaches is a configuration
	// mistake, and failing every request for that capability is a worse answer
	// than serving it with a sane budget and saying so in the log.
	MaxTimeout  = 30 * time.Second
	MaxRetryMax = 5
)

// How long this step waits between attempts.
const (
	// RetryBackoffBase is the first wait between attempts, doubling from there
	// up to RetryBackoffMax. Short, because the retry budget comes from the
	// registry and an operator setting 5 retries did not ask for seconds of
	// latency -- only for the provider's brief unavailability to be ridden out.
	RetryBackoffBase = 50 * time.Millisecond
	RetryBackoffMax  = 800 * time.Millisecond
)

// redactedMarker stands in for a credential in anything logged or returned.
const redactedMarker = "REDACTED"

// Auth schemes this step can present upstream. Credentials themselves are never
// configured here or held in the registry -- config names the environment
// variable to read, so a secret reaches the process through its environment and
// nothing else.
const (
	AuthSchemeNone   = "none"
	AuthSchemeBasic  = "basic"
	AuthSchemeHeader = "header"
	// AuthSchemeQuery puts the credential in the query string, which some
	// upstreams are built around whatever anyone thinks of it. It is the least
	// safe of the four -- a query string is logged by proxies and appears in a
	// transport error -- so the value is redacted from anything this package
	// logs or returns. See redact.
	AuthSchemeQuery = "query"

	// AuthSchemeOAuth2 exchanges a client id and secret for a short-lived
	// bearer token, and sends that. For an upstream behind an OAuth2 token
	// endpoint -- a Keycloak service account, say -- where a static token is not
	// an option: the live one this was written against lives ten hours, so a
	// value pasted into an environment variable is wrong twice a day.
	//
	// Only the client_credentials grant. There is no user to redirect and no
	// refresh token in that grant, so the other flows would be dead code.
	AuthSchemeOAuth2 = "oauth2"
)

// codeUpstreamUnavailable reports a provider that could not be reached or
// answered with a failure. It is not this adapter's fault and not the caller's.
const codeUpstreamUnavailable = "NET_DOWNSTREAM_UNAVAILABLE"

// tokenRefreshSkew is how early an oauth2 token stops being trusted. It has to
// exceed the round trip to the provider, so that a request which passes the
// expiry check cannot arrive after the token has actually died. One extra
// exchange per token lifetime is the whole cost.
const tokenRefreshSkew = 60 * time.Second

// explainLimit is how much of a failed response is quoted. Enough for a
// provider's own message, short enough not to put a page of HTML in a log line
// or a NACK.
const explainLimit = 300
