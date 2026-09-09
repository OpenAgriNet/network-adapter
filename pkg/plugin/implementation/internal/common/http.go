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

// call makes the upstream request described by the plan, retrying within its
// budget.
// budget resolves how long one attempt may take and how many retries follow
// it, applying the registry's values within this deployment's ceilings.
//
// Pure and separate from call so both bounds can be asserted without a server
// that sleeps for the timeout it is testing.
//
// retryMax counts retries, not attempts, so the call itself is always made
// once. An absent retryMax and an explicit 0 are the same instruction.
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

func (s *Step) call(ctx context.Context, auth *authenticator, baseURL string, call model.ActionPlan, mapped []byte) ([]byte, error) {
	endpoint, err := buildEndpoint(baseURL, call, mapped)
	if err != nil {
		return nil, err
	}

	timeout, retries := budget(call)
	if d := time.Duration(call.TimeoutMs) * time.Millisecond; d > timeout {
		log.Warnf(ctx, "upstream: registry asks for a %v timeout; using the %v ceiling", d, timeout)
	}
	if call.RetryMax > retries {
		log.Warnf(ctx, "upstream: registry asks for %d retries; using the %d ceiling",
			call.RetryMax, retries)
	}
	attempts := retries + 1

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		// A caller that has gone away is not worth another attempt, and neither
		// is a budget already spent. Checked before the call rather than after,
		// so a cancelled request costs nothing.
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
		log.Warnf(ctx, "upstream: attempt %d/%d failed: %v", attempt, attempts, lastErr)

		// Only some failures are worth repeating. A 4xx, a request this step
		// could not build and a credential it could not read will fail
		// identically however many times they are tried -- and retrying the
		// credential case is the worst of them, because it reports an
		// operator's missing environment variable as the provider being down.
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
		fmt.Errorf("upstream: provider did not answer after %d attempts: %w", attempts, lastErr))
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
// Exponential from a short base and capped, because the provider being briefly
// busy is the case worth waiting out; anything longer is a timeout's job. With
// no wait at all a retryMax of 5 spends its whole budget inside a couple of
// milliseconds, which is not a retry so much as the same failure six times.
func backoff(attempt int) time.Duration {
	if attempt <= 1 {
		return RetryBackoffBase
	}
	// Doubled in a loop that stops at the ceiling rather than shifted and then
	// clamped. `RetryBackoffBase << (attempt - 1)` overflows int64 once the
	// shift reaches 38 at a 50ms base, and the wrapped value is NEGATIVE -- so
	// it passes the `> RetryBackoffMax` check, is returned, and a sleep on a
	// negative duration returns immediately. The retry loop then spins as fast
	// as the provider can refuse. Stopping at the ceiling cannot overflow,
	// because it never doubles a value already at or past it.
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

	// The URL as it actually went on the wire, credential removed. At info
	// rather than debug because this is the line that answers "what did we ask,
	// and what came back" -- the question every provider problem starts with.
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
	log.Infof(ctx, "upstream: %s %s -> %s, %d bytes", method, requested, resp.Status, len(body))
	if int64(len(body)) > s.config.MaxResponseBytes {
		// Asking again will not make the answer smaller.
		return nil, doNotRetry(fmt.Errorf("response exceeds the %d byte limit", s.config.MaxResponseBytes))
	}
	// Any 2xx, not 200 alone. A provider is entitled to answer 202 for work it
	// accepted, 204 for nothing to report, or 201 for something it created, and
	// treating those as failures would refuse a perfectly good exchange. 3xx
	// does not reach here: the client follows redirects.
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		// The body is logged, not returned. It goes into a 502 that is signed
		// and sent to the network caller, and what a provider puts in a failure
		// body is its own business -- a stack trace, an internal hostname, a
		// database error. The status is the caller's business and stays; the
		// body is the operator's, and the log is where the operator looks.
		// Redacted on the way to the log too. A provider that rejects a
		// request often quotes it back, credential and all -- so the body is
		// exactly where a query-string token turns up, and moving it from the
		// error to the log would only move the leak.
		log.Warnf(ctx, "upstream: provider returned %s for %s %s: %s",
			resp.Status, method, requested, s.redactString(explain(body)))
		err := fmt.Errorf("provider returned %s", resp.Status)
		// 5xx and 429 are the provider asking to be tried again. Every other
		// 4xx is a statement about the request, which will not improve.
		if resp.StatusCode < http.StatusInternalServerError && resp.StatusCode != http.StatusTooManyRequests {
			return nil, doNotRetry(err)
		}
		return nil, err
	}
	return body, nil
}

// explain renders a failed response body for a human.
//
// The body was already read and then thrown away, so a provider's own account
// of what was wrong -- Agmarknet says "no data" in the body of a 400 -- never
// reached anyone. The status alone says a call failed and nothing about why,
// which is the first thing an operator needs and the thing that makes a real
// provider's behaviour observable at all.
func explain(body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return "(no body)"
	}
	// Collapse whitespace: a provider that answers with indented JSON or an
	// HTML error page should not spread one failure over forty log lines.
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

	// baseUrl cannot end in a slash and path must begin with one, so exactly one
	// separator appears between them. The trim is belt and braces: the registry
	// refuses a trailing slash on baseUrl, and this keeps a row that predates
	// that from producing a doubled one.
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
// The registry constrains this, but it is a separate deployable that may not be
// updated in step, so a row that slipped through has to fail here with something
// an operator can act on rather than as a provider's 404 three hops away.
//
// An empty segment is the case worth catching: "//get-daily" is never
// deliberate, and plenty of servers answer it differently from "/get-daily". A
// trailing slash is deliberately left alone -- "/api/" and "/api" are a
// distinction some APIs genuinely make, so stripping it would silently change
// the URL the operator published.
func verifyPath(path string) error {
	if path == "" {
		return model.NewBadReqErr("", errors.New("upstream: the registry publishes no path for this action"))
	}
	if !strings.HasPrefix(path, "/") {
		return model.NewBadReqErr("", fmt.Errorf(
			"upstream: path %q does not begin with a slash, so it cannot be joined to a base url", path))
	}
	if strings.Contains(path, "//") {
		return model.NewBadReqErr("", fmt.Errorf(
			"upstream: path %q has an empty segment; write it with single slashes", path))
	}
	// A dot segment is refused rather than resolved. The registry says which
	// path answers an action, and a row that climbs out of it is either a
	// mistake or an attempt to reach something the row does not name -- and
	// net/url would quietly resolve it either way, so the request that left
	// would not be the request the row described.
	for _, segment := range strings.Split(path, "/") {
		if segment == ".." || segment == "." {
			return model.NewBadReqErr("", fmt.Errorf(
				"upstream: path %q contains the %q segment; publish the path it resolves to instead",
				path, segment))
		}
	}
	// A fragment is never sent, so a row carrying one describes a request that
	// cannot be made. Refused here rather than silently dropped by the
	// transport, which would make the row look honoured.
	if strings.Contains(path, "#") {
		return model.NewBadReqErr("", fmt.Errorf(
			"upstream: path %q contains a fragment, which is never sent to a server", path))
	}
	return nil
}

// verifyBaseURL checks the participant's base url before it is joined to a
// path, so a row that cannot produce a request says so as a bad request rather
// than as the provider being unreachable.
//
// Without this, `baseUrl: "registry:8081"` -- a scheme left off -- failed
// inside http.NewRequestWithContext and arrived as a 502 "provider did not
// answer after 1 attempts: could not build the request". That names the
// provider for an error in the row describing it, and it is retried on the way
// there. jsonmapper has always checked its own reference this way; this is the
// same check on the other url the registry publishes.
func verifyBaseURL(baseURL string) error {
	if baseURL == "" {
		return model.NewBadReqErr("", errors.New("upstream: the registry publishes no base url for this provider"))
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return model.NewBadReqErr("", fmt.Errorf("upstream: invalid base url %q: %w", baseURL, err))
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return model.NewBadReqErr("", fmt.Errorf(
			"upstream: base url %q must be http or https", baseURL))
	}
	if parsed.Host == "" {
		return model.NewBadReqErr("", fmt.Errorf("upstream: base url %q names no host", baseURL))
	}
	return nil
}

// asQuery renders a mapped request as query parameters.
//
// A method with no body still needs the mapping's output somewhere, and the
// query string is the only place it can go. Only scalars are carried: a nested
// value has no single obvious encoding, and inventing one here would put a
// convention in Go that belongs in the mapping.
func asQuery(mapped []byte) (string, error) {
	if len(bytes.TrimSpace(mapped)) == 0 {
		return "", nil
	}
	var fields map[string]any
	if err := json.Unmarshal(mapped, &fields); err != nil {
		return "", fmt.Errorf("upstream: mapped request is not an object, so it cannot become a query: %w", err)
	}

	values := url.Values{}
	for name, value := range fields {
		rendered, ok := renderScalar(value)
		if !ok {
			return "", fmt.Errorf("upstream: mapped field %q is not a scalar and cannot become a query parameter", name)
		}
		values.Set(name, rendered)
	}
	return values.Encode(), nil
}

// renderScalar renders a JSON scalar as a query parameter value.
func renderScalar(value any) (string, bool) {
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

// canonicalMethod returns a known HTTP method in the spelling the RFC gives
// it, and anything else unchanged.
//
// hasBody used to upper-case privately, which made the method look
// case-insensitive when it is not: NewRequestWithContext transmits it verbatim,
// so a registry row reading `method: "post"` sent `post /path HTTP/1.1`. The
// body was attached correctly -- hasBody had normalised -- but nginx and most
// gateways answer 405 to a lowercase method, which classifies permanent and
// surfaces as a 502 "provider did not answer". A row that is right in every
// respect but its capitalisation is a bad way to spend an afternoon.
//
// Only known methods are rewritten. Upper-casing everything would be a new
// restriction on an upstream entitled to a method this list has not heard of,
// and net/http already refuses one that is not a valid token.
//
// An empty method is left empty: net/http documents "" as GET and substitutes
// it, and hasBody agrees that it carries no body, so the two are already
// consistent and inventing a value here would only hide where it comes from.
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
