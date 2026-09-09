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
func (s *Step) attempt(ctx context.Context, auth *authenticator, call model.ActionPlan, endpoint string, mapped []byte, timeout time.Duration) ([]byte, error) {
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
	if err := s.authenticate(auth, req); err != nil {
		// A missing or unreadable credential is configuration, not weather.
		return nil, util.DoNotRetry(err)
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
		return nil, util.DoNotRetry(fmt.Errorf("response exceeds the %d byte limit", s.config.MaxResponseBytes))
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
			resp.Status, method, requested, s.redactString(util.Explain(body)))
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
