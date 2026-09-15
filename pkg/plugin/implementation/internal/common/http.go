// Talking HTTP to a provider: building the endpoint and body, the attempt and
// its retry budget, and reading what came back.
package common

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
)

// call makes the upstream request the plan describes, retrying within util.Budget.
func (s *Step) call(ctx context.Context, auth *authenticator, baseURL string, call model.ActionPlan, mapped []byte) ([]byte, error) {
	endpoint, err := util.BuildEndpoint(baseURL, call, mapped)
	if err != nil {
		return nil, err
	}

	timeout, retries := util.Budget(call)
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

		body, err := s.attempt(ctx, auth, call, endpoint, mapped, timeout, attempt, attempts)
		if err == nil {
			return body, nil
		}
		lastErr = s.redact(err)
		log.Warnf(ctx, "attempt %d/%d failed: %v", attempt, attempts, lastErr)

		// A 4xx, an unbuildable request and an unreadable credential fail
		// identically however often they are tried. Retrying the credential
		// case is the worst: it reports a missing environment variable as the
		// provider being down.
		if util.IsPermanent(err) {
			break
		}
		if attempt < attempts {
			if err := util.Sleep(ctx, util.Backoff(attempt)); err != nil {
				break
			}
		}
	}
	return nil, model.NewCodedErr(http.StatusBadGateway, util.CodeUpstreamUnavailable,
		fmt.Errorf("provider did not answer after %d attempts: %w", attempts, lastErr))
}

// attempt makes one upstream request.
func (s *Step) attempt(ctx context.Context, auth *authenticator, call model.ActionPlan, endpoint string, mapped []byte, timeout time.Duration, attempt, attempts int) ([]byte, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	method := util.CanonicalMethod(call.Method)
	req, err := http.NewRequestWithContext(attemptCtx, method, endpoint, util.RequestBody(method, mapped))
	if err != nil {
		return nil, util.DoNotRetry(fmt.Errorf("could not build the request: %w", err))
	}
	if util.HasBody(method) {
		req.Header.Set("Content-Type", "application/json")
	}
	// The span opens here rather than at the top of attempt: everything above
	// is building a request, and timing that would report our own work as the
	// provider's.
	//
	// It opens BEFORE authenticate, not after, for two reasons. An oauth2 or
	// tokenQuery scheme dials a token endpoint from in there, and a caller
	// waits on that exactly as it waits on the provider. And a credential that
	// cannot be read fails there -- if the span started afterwards, the one
	// failure an operator can actually fix would be the only one that emitted
	// no telemetry at all.
	//
	// Seeded with the pre-credential endpoint: a query-scheme credential is
	// added to the URL inside authenticate, so req.URL is not yet safe to
	// record. The redacted one replaces it below.
	_, observed := startProviderCall(attemptCtx, method, endpoint, attempt, attempts)

	if err := s.authenticate(auth, req); err != nil {
		// Already classified at the source. A missing environment variable is
		// configuration and marked permanent there; an oauth2 exchange that
		// could not reach the issuer is weather and is not, so the retry budget
		// covers the token endpoint exactly as it covers the provider.
		observed.done(ctx, 0, 0, err)
		return nil, err
	}

	// The URL as it went on the wire, credential removed. At info because this
	// is the line that answers "what did we ask, and what came back".
	requested := s.redactString(req.URL.String())
	observed.setURL(requested)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		observed.done(ctx, 0, 0, err)
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, s.config.MaxResponseBytes+1))
	if err != nil {
		// Recorded at the status the provider gave: it answered, we could not
		// read what it said.
		observed.done(ctx, resp.StatusCode, 0, err)
		return nil, fmt.Errorf("could not read the response: %w", err)
	}

	// Read once, before done() stops the span, so the duration on the log line
	// and the one in the histogram are the same measurement rather than two
	// that disagree by however long classification took.
	//
	// It covers the response read, not just the round trip. A provider that
	// answers instantly and then dribbles a megabyte is slow, and a number
	// that stopped at the headers would call it fast.
	took := observed.elapsed()
	log.Infof(ctx, "%s %s -> %s, %d bytes in %s",
		method, requested, resp.Status, len(body), took.Round(time.Millisecond))

	if int64(len(body)) > s.config.MaxResponseBytes {
		// Asking again will not make the answer smaller.
		err := util.DoNotRetry(fmt.Errorf("response exceeds the %d byte limit", s.config.MaxResponseBytes))
		observed.done(ctx, resp.StatusCode, len(body), err)
		return nil, err
	}
	observed.done(ctx, resp.StatusCode, len(body), nil)
	// Any 2xx, not 200 alone: 202, 204 and 201 are all legitimate answers. 3xx
	// does not reach here, since the client follows redirects.
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		// The body is logged, not returned. It would go into a signed 502, and
		// a failure body may hold a stack trace or an internal hostname. The
		// status is the caller's business; the body is the operator's.
		//
		// Redacted on the way to the log too: a rejected request is often
		// quoted back, credential and all.
		log.Warnf(ctx, "provider returned %s for %s %s in %s: %s",
			resp.Status, method, requested, took.Round(time.Millisecond),
			s.redactString(util.Explain(body)))

		// A held token the provider has stopped accepting is dropped, so the
		// next call exchanges a fresh one.
		//
		// This is what keeps a wrong tokenTtl from being an outage. The
		// lifetime is an operator's ESTIMATE -- the endpoint states no expiry
		// -- so a too-generous one leaves a dead token cached, and without this
		// every call would fail until it lapsed. One request pays; the next
		// recovers.
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			s.forgetToken(auth)
		}

		err := fmt.Errorf("provider returned %s", resp.Status)
		// 5xx and 429 ask to be tried again. Every other 4xx is a statement
		// about the request, which will not improve.
		if resp.StatusCode < http.StatusInternalServerError && resp.StatusCode != http.StatusTooManyRequests {
			return nil, util.DoNotRetry(err)
		}
		return nil, err
	}
	return body, nil
}
