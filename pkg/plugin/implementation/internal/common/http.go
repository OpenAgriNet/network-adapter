// Talking HTTP to a provider: building the endpoint and body, the attempt and
// its retry budget, and reading what came back.
package common

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
)

// budget resolves how long one attempt may take and how many retries follow,
// applying the registry's values within this deployment's ceilings.
//
// Separate from call so both bounds can be asserted without a server that
// sleeps for the timeout being tested.
//
// retryMax counts retries, not attempts: the call is always made once, and an
// absent retryMax means the same as an explicit 0.
func budget(call model.ActionPlan) (time.Duration, int) {
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

// call makes the upstream request the plan describes, retrying within budget.
func (s *Step) call(ctx context.Context, auth *authenticator, baseURL string, call model.ActionPlan, mapped []byte) ([]byte, error) {
	endpoint, err := buildEndpoint(baseURL, call, mapped)
	if err != nil {
		return nil, err
	}

	timeout, retries := budget(call)
	if d := time.Duration(call.TimeoutMs) * time.Millisecond; d > timeout {
		log.Warnf(ctx, "registry asks for a %v timeout; using the %v ceiling", d, timeout)
	}
	if call.RetryMax > retries {
		log.Warnf(ctx, "registry asks for %d retries; using the %d ceiling",
			call.RetryMax, retries)
	}
	attempts := retries + 1

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		// Checked before the call, so a cancelled request costs nothing.
		if err := ctx.Err(); err != nil {
			if lastErr == nil {
				lastErr = err
			}
			break
		}

		body, err := s.attempt(ctx, auth, call, endpoint, mapped, timeout)
		if err == nil {
			return body, nil
		}
		lastErr = s.redact(err)
		log.Warnf(ctx, "attempt %d/%d failed: %v", attempt, attempts, lastErr)

		// A 4xx, an unbuildable request and an unreadable credential fail
		// identically however often they are tried. Retrying the credential
		// case is the worst: it reports a missing environment variable as the
		// provider being down.
		if isPermanent(err) {
			break
		}
		if attempt < attempts {
			if err := sleep(ctx, backoff(attempt)); err != nil {
				break
			}
		}
	}
	return nil, model.NewCodedErr(http.StatusBadGateway, codeUpstreamUnavailable,
		fmt.Errorf("provider did not answer after %d attempts: %w", attempts, lastErr))
}

// permanentErr marks a failure no retry can fix. Kept unexported and detected
// with errors.As, so a caller of this package sees only the underlying error.
type permanentErr struct{ error }

func (p permanentErr) Unwrap() error { return p.error }

// doNotRetry marks err as not worth repeating.
func doNotRetry(err error) error { return permanentErr{err} }

// isPermanent reports whether err is one no further attempt would change.
func isPermanent(err error) bool {
	var permanent permanentErr
	return errors.As(err, &permanent)
}

// backoff is how long to wait before the next attempt.
//
// Exponential from a short base and capped: a briefly busy provider is worth
// waiting out, anything longer is a timeout's job. With no wait, retryMax 5
// spends its budget in a couple of milliseconds -- the same failure six times.
func backoff(attempt int) time.Duration {
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

// sleep waits, or reports that the context ended first.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// attempt makes one upstream request.
func (s *Step) attempt(ctx context.Context, auth *authenticator, call model.ActionPlan, endpoint string, mapped []byte, timeout time.Duration) ([]byte, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	method := canonicalMethod(call.Method)
	req, err := http.NewRequestWithContext(attemptCtx, method, endpoint, requestBody(method, mapped))
	if err != nil {
		return nil, doNotRetry(fmt.Errorf("could not build the request: %w", err))
	}
	if hasBody(method) {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := s.authenticate(auth, req); err != nil {
		// A missing or unreadable credential is configuration, not weather.
		return nil, doNotRetry(err)
	}

	// The URL as it went on the wire, credential removed. At info because this
	// is the line that answers "what did we ask, and what came back".
	requested := s.redactString(req.URL.String())

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, s.config.MaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("could not read the response: %w", err)
	}
	log.Infof(ctx, "%s %s -> %s, %d bytes", method, requested, resp.Status, len(body))
	if int64(len(body)) > s.config.MaxResponseBytes {
		// Asking again will not make the answer smaller.
		return nil, doNotRetry(fmt.Errorf("response exceeds the %d byte limit", s.config.MaxResponseBytes))
	}
	// Any 2xx, not 200 alone: 202, 204 and 201 are all legitimate answers. 3xx
	// does not reach here, since the client follows redirects.
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		// The body is logged, not returned. It would go into a signed 502, and
		// a failure body may hold a stack trace or an internal hostname. The
		// status is the caller's business; the body is the operator's.
		//
		// Redacted on the way to the log too: a rejected request is often
		// quoted back, credential and all.
		log.Warnf(ctx, "provider returned %s for %s %s: %s",
			resp.Status, method, requested, s.redactString(explain(body)))
		err := fmt.Errorf("provider returned %s", resp.Status)
		// 5xx and 429 ask to be tried again. Every other 4xx is a statement
		// about the request, which will not improve.
		if resp.StatusCode < http.StatusInternalServerError && resp.StatusCode != http.StatusTooManyRequests {
			return nil, doNotRetry(err)
		}
		return nil, err
	}
	return body, nil
}

// explain renders a failed response body for a human.
//
// The status says a call failed and nothing about why. The provider's own
// account is in the body -- Agmarknet says "no data" in the body of a 400.
func explain(body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return "(no body)"
	}
	// Collapse whitespace: indented JSON or an HTML error page should not
	// spread one failure over forty log lines.
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > explainLimit {
		return text[:explainLimit] + "... (truncated)"
	}
	return text
}

// buildEndpoint joins the plan's base URL and path, carrying the mapped request
// as query parameters when the method takes no body.
func buildEndpoint(baseURL string, call model.ActionPlan, mapped []byte) (string, error) {
	if err := verifyBaseURL(baseURL); err != nil {
		return "", err
	}
	if err := verifyPath(call.Path); err != nil {
		return "", err
	}

	// baseUrl cannot end in a slash and path must begin with one. The trim is
	// belt and braces, for a row predating that rule.
	endpoint := strings.TrimSuffix(baseURL, "/") + call.Path
	if hasBody(call.Method) {
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

// asQuery renders a mapped request as query parameters, for a method with no
// body. Scalars only -- see asQueryValue.
func asQuery(mapped []byte) (string, error) {
	if len(bytes.TrimSpace(mapped)) == 0 {
		return "", nil
	}
	var fields map[string]any
	if err := json.Unmarshal(mapped, &fields); err != nil {
		return "", fmt.Errorf("mapped request is not an object, so it cannot become a query: %w", err)
	}

	values := url.Values{}
	for name, value := range fields {
		rendered, ok := asQueryValue(value)
		if !ok {
			return "", fmt.Errorf("mapped field %q is not a scalar and cannot become a query parameter", name)
		}
		values.Set(name, rendered)
	}
	return values.Encode(), nil
}

// asQueryValue renders one mapped field as a query parameter value, reporting
// false when the field cannot be one.
//
// The three JSON scalars and nothing else. An object or array has no single
// obvious encoding -- repeated keys, comma-joined, indexed are all in use
// somewhere -- so choosing here would put a convention in Go that belongs in
// the mapping. The caller turns false into an error naming the field.
//
// null is refused rather than sent as empty: present-but-empty and absent mean
// different things to some upstreams, and a mapping says which by omitting the
// field or setting "". An empty string IS carried, being a scalar.
func asQueryValue(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case bool:
		return strconv.FormatBool(typed), true
	case float64:
		// 'g' with -1 precision round-trips without inventing trailing zeros, so
		// 19.9975 stays 19.9975 rather than becoming 19.997500.
		return strconv.FormatFloat(typed, 'g', -1, 64), true
	default:
		return "", false
	}
}

// requestBody returns the body to send, which is none for methods that take none.
func requestBody(method string, mapped []byte) io.Reader {
	if !hasBody(method) {
		return nil
	}
	return bytes.NewReader(mapped)
}

// hasBody reports whether a method carries a request body.
func hasBody(method string) bool {
	switch canonicalMethod(method) {
	case http.MethodGet, http.MethodHead, http.MethodDelete, "":
		return false
	default:
		return true
	}
}

// canonicalMethod upper-cases a known HTTP method and leaves anything else
// alone.
//
// NewRequestWithContext sends the method verbatim, so a row reading "post" put
// `post /path HTTP/1.1` on the wire and most gateways answer that with 405 --
// which classifies permanent and surfaces as "provider did not answer".
//
// Unknown methods pass through: net/http already rejects an invalid token, and
// upper-casing everything would restrict an upstream entitled to a method this
// list has not heard of. An empty method stays empty, since net/http already
// treats "" as GET.
func canonicalMethod(method string) string {
	upper := strings.ToUpper(method)
	switch upper {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodConnect,
		http.MethodOptions, http.MethodTrace:
		return upper
	}
	return method
}
