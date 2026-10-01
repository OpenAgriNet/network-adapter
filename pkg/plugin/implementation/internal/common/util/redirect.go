package util

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ErrRedirectRefused is the reason a call failed when the upstream redirected
// somewhere a credential-bearing client will not follow.
//
// A sentinel so the reason survives opaque wrapping: without it a refused
// redirect reads as "could not be reached", and an operator chases a network
// fault that is not there.
var ErrRedirectRefused = errors.New("the upstream redirected off-host and was not followed, " +
	"because every request here carries a credential")

// maxRedirects is Go's own default. Setting CheckRedirect replaces that
// default, so the limit has to be kept here.
const maxRedirects = 10

// RefuseOffHostRedirect is the CheckRedirect for a client whose every request
// carries a credential -- in a header, a body, or the query string. A 307/308
// preserves method and body, and Go does not strip a query-string credential
// on a cross-host redirect the way it strips sensitive headers, so following
// one would hand the credential to wherever the response points.
//
// A redirect that stays on the same host and scheme (a trailing slash, a
// moved path) is ordinary and is followed. An https-to-http downgrade on the
// same host is refused: it would send the credential in cleartext.
func RefuseOffHostRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", len(via))
	}
	from := via[len(via)-1].URL
	if strings.EqualFold(req.URL.Host, from.Host) && !isSchemeDowngrade(from.Scheme, req.URL.Scheme) {
		return nil
	}
	// The host and scheme are named, the URL is not: a redirect target on the
	// data path would carry the credential in its query string.
	return fmt.Errorf("%w to %s://%s", ErrRedirectRefused, req.URL.Scheme, req.URL.Host)
}

// isSchemeDowngrade reports an https request being redirected to cleartext.
func isSchemeDowngrade(from, to string) bool {
	return strings.EqualFold(from, "https") && !strings.EqualFold(to, "https")
}
