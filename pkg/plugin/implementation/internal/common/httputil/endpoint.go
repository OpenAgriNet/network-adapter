// Building the URL a request goes to, and refusing one a registry row could
// not have meant.
package httputil

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

// BuildEndpoint joins the plan's base URL and path, carrying the mapped request
// as query parameters when the method takes no body.
func BuildEndpoint(baseURL string, call model.ActionPlan, mapped []byte) (string, error) {
	if err := verifyBaseURL(baseURL); err != nil {
		return "", err
	}
	if err := verifyPath(call.Path); err != nil {
		return "", err
	}

	// baseUrl cannot end in a slash and path must begin with one. The trim is
	// belt and braces, for a row predating that rule.
	endpoint := strings.TrimSuffix(baseURL, "/") + call.Path
	if HasBody(call.Method) {
		return endpoint, nil
	}

	query, err := asQuery(mapped)
	if err != nil {
		return "", err
	}
	if query == "" {
		return endpoint, nil
	}
	if strings.Contains(endpoint, "?") {
		return endpoint + "&" + query, nil
	}
	return endpoint + "?" + query, nil
}

// verifyPath refuses a published path nobody could have meant.
//
// The registry constrains this, but it deploys separately, so a row that slips
// through must fail here with something an operator can act on rather than as
// a 404 three hops away.
//
// An empty segment is the case worth catching: "//get-daily" is never
// deliberate. A trailing slash is left alone -- "/api/" and "/api" are a
// distinction some APIs genuinely make.
func verifyPath(path string) error {
	if path == "" {
		return model.NewBadReqErr("", errors.New("the registry publishes no path for this action"))
	}
	if !strings.HasPrefix(path, "/") {
		return model.NewBadReqErr("", fmt.Errorf(
			"path %q does not begin with a slash, so it cannot be joined to a base url", path))
	}
	if strings.Contains(path, "//") {
		return model.NewBadReqErr("", fmt.Errorf(
			"path %q has an empty segment; write it with single slashes", path))
	}
	// A dot segment is refused, not resolved: net/url would resolve it quietly,
	// and the request that left would not be the request the row described.
	for _, segment := range strings.Split(path, "/") {
		if segment == ".." || segment == "." {
			return model.NewBadReqErr("", fmt.Errorf(
				"path %q contains the %q segment; publish the path it resolves to instead",
				path, segment))
		}
	}
	// A fragment is never sent, so a row carrying one describes a request that
	// cannot be made. The transport would drop it silently and the row would
	// look honoured.
	if strings.Contains(path, "#") {
		return model.NewBadReqErr("", fmt.Errorf(
			"path %q contains a fragment, which is never sent to a server", path))
	}
	return nil
}

// verifyBaseURL checks the base url before it is joined to a path, so a row
// that cannot produce a request says so as a bad request rather than as the
// provider being unreachable.
//
// Without this, a missing scheme (`baseUrl: "registry:8081"`) failed inside
// NewRequestWithContext and arrived as a 502 blaming the provider -- retried
// on the way there.
func verifyBaseURL(baseURL string) error {
	if baseURL == "" {
		return model.NewBadReqErr("", errors.New("the registry publishes no base url for this provider"))
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return model.NewBadReqErr("", fmt.Errorf("invalid base url %q: %w", baseURL, err))
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return model.NewBadReqErr("", fmt.Errorf(
			"base url %q must be http or https", baseURL))
	}
	if parsed.Host == "" {
		return model.NewBadReqErr("", fmt.Errorf("base url %q names no host", baseURL))
	}
	return nil
}
