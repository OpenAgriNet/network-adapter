// Constants only. authFields is not here: it is a var, since Go has no constant
// maps, and it sits beside the parser in auth.go that reads it.
package common

import (
	"time"
)

// DefaultMaxResponseBytes caps what is read from a provider. The response is
// mapped in memory, so an unbounded one is an unbounded allocation.
const DefaultMaxResponseBytes = 4 << 20 // 4 MiB

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
