// Talking HTTP to a provider: building the endpoint and body, the attempt and
// its retry budget, and reading what came back.
package common

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
	"github.com/beckn-one/beckn-onix/pkg/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// call makes the upstream request the plan describes, retrying within util.Budget.
func (s *Step) call(ctx context.Context, auth *authenticator, provider, baseURL string, call model.ActionPlan, mapped []byte) ([]byte, error) {
	endpoint, err := util.BuildEndpoint(baseURL, call, mapped)
	if err != nil {
		return nil, notConfiguredErr(provider, err)
	}

	// Finish before the request's own deadline, so the answer still reaches a
	// hop that forwarded this request and is waiting on the same budget.
	if deadline, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline.Add(-util.AnswerMargin))
		defer cancel()
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
	var made int
	// final is set when the provider's answer ended the loop: that answer is
	// the result, even if the caller left at the same moment.
	var final bool
	for attempt := 1; attempt <= attempts; attempt++ {
		// Checked before the call, so a cancelled request costs nothing -- and
		// before counting it, so an attempt that never ran is not reported.
		if ctx.Err() != nil {
			break
		}
		made = attempt

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
			final = true
			break
		}
		if attempt < attempts {
			if err := util.Sleep(ctx, util.Backoff(attempt)); err != nil {
				break
			}
		}
	}
	// The request ended (a caller that left, or a deadline) before the retries
	// were done. That is not the provider failing, so it is not a 502, and the
	// context's own error stays classifiable. What the provider did answer, if
	// anything, is kept beside it.
	if ended := ctx.Err(); ended != nil && !final {
		cause := fmt.Errorf("request ended after %s, before the provider answered: %w", attemptCount(made), ended)
		if lastErr != nil && !errors.Is(lastErr, context.Canceled) && !errors.Is(lastErr, context.DeadlineExceeded) {
			cause = fmt.Errorf("request ended after %s: %w; the provider's last answer: %w", attemptCount(made), ended, lastErr)
		}
		return nil, model.NewCodedErr(http.StatusGatewayTimeout, util.CodeTimeout, cause).
			WithMessage(fmt.Sprintf("%s did not answer before the request ended", provider))
	}
	return nil, callFailure(provider, made, timeout,
		fmt.Errorf("provider did not answer after %s: %w", attemptCount(made), lastErr))
}

// Failures marked where they happen, so callFailure can tell the caller which
// one it was without reading error text.
var (
	errNotConfigured      = errors.New("this adapter is not configured to call the provider")
	errTokenExchange      = errors.New("token exchange failed")
	errUnreadableResponse = errors.New("the provider's response could not be read")
)

// upstreamStatusError is a provider answering with a non-2xx status.
type upstreamStatusError struct {
	statusCode int
	status     string
}

func (e *upstreamStatusError) Error() string { return "provider returned " + e.status }

// callFailure is what the caller is told when no attempt succeeded. err stays
// whole for the log; the message names only the provider and what went wrong.
func callFailure(provider string, attempts int, timeout time.Duration, err error) error {
	var answered *upstreamStatusError
	switch {
	case errors.Is(err, errNotConfigured):
		return notConfiguredErr(provider, err)
	case errors.Is(err, errTokenExchange):
		return model.NewCodedErr(http.StatusBadGateway, util.CodeUpstreamUnavailable, err).
			WithMessage("could not get a login token from " + provider)
	case errors.As(err, &answered):
		return providerAnswerErr(provider, attempts, answered.statusCode, err)
	case errors.Is(err, errUnreadableResponse):
		return unreadableResponseErr(provider, err)
	case errors.Is(err, context.DeadlineExceeded):
		return model.NewCodedErr(http.StatusGatewayTimeout, util.CodeTimeout, err).
			WithMessage(fmt.Sprintf("%s did not respond within %s (%s)", provider, timeout, attemptCount(attempts)))
	default:
		return model.NewCodedErr(http.StatusBadGateway, util.CodeUpstreamUnavailable, err).
			WithMessage(fmt.Sprintf("%s is not reachable after %s", provider, attemptCount(attempts)))
	}
}

// providerAnswerErr maps the provider's error status to the caller's. A 400 or
// 404 is about the request, so the caller gets the same status; anything else
// is the provider failing, so 502.
func providerAnswerErr(provider string, attempts, statusCode int, err error) error {
	switch statusCode {
	case http.StatusBadRequest:
		return model.NewCodedErr(http.StatusBadRequest, util.CodeProviderRefused, err).
			WithMessage(provider + " rejected the request")
	case http.StatusNotFound:
		return model.NewCodedErr(http.StatusNotFound, util.CodeNotFound, err).
			WithMessage(provider + " has no matching record")
	case http.StatusUnauthorized, http.StatusForbidden:
		return model.NewCodedErr(http.StatusBadGateway, util.CodeUpstreamUnavailable, err).
			WithMessage(provider + " rejected the adapter's credentials")
	default:
		return model.NewCodedErr(http.StatusBadGateway, util.CodeUpstreamUnavailable, err).
			WithMessage(fmt.Sprintf("%s returned %d after %s", provider, statusCode, attemptCount(attempts)))
	}
}

// unreadableResponseErr is the 502 for an answer that is not what a provider
// sends: not JSON, empty, or too large.
func unreadableResponseErr(provider string, err error) error {
	return model.NewCodedErr(http.StatusBadGateway, util.CodeUnreadableAnswer, err).
		WithMessage(provider + " sent a response that could not be read")
}

// notConfiguredErr is the 500 for a provider this adapter cannot call as set
// up: a missing credential or a registry record that describes no request.
func notConfiguredErr(provider string, err error) error {
	return model.NewCodedErr(http.StatusInternalServerError, util.CodeInternalError, err).
		WithMessage(provider + " is not configured on this adapter")
}

// attemptCount reads "1 attempt" or "3 attempts".
func attemptCount(count int) string {
	if count == 1 {
		return "1 attempt"
	}
	return fmt.Sprintf("%d attempts", count)
}

// attempt makes one upstream request.
func (s *Step) attempt(ctx context.Context, auth *authenticator, call model.ActionPlan, endpoint string, mapped []byte, timeout time.Duration) ([]byte, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	method := util.CanonicalMethod(call.Method)
	req, err := http.NewRequestWithContext(attemptCtx, method, endpoint, util.RequestBody(method, mapped))
	if err != nil {
		return nil, util.DoNotRetry(fmt.Errorf("%w: could not build the request: %w", errNotConfigured, err))
	}
	if util.HasBody(method) {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := s.authenticate(auth, req); err != nil {
		// Already classified at the source. A missing environment variable is
		// configuration and marked permanent there; an oauth2 exchange that
		// could not reach the issuer is weather and is not, so the retry budget
		// covers the token endpoint exactly as it covers the provider.
		return nil, err
	}

	// The URL as it went on the wire, credential removed. At info because this
	// is the line that answers "what did we ask, and what came back".
	requested := s.redactString(req.URL.String())

	tracer := otel.Tracer(telemetry.ScopeName, trace.WithInstrumentationVersion(telemetry.ScopeVersion))
	httpCtx, httpSpan := tracer.Start(attemptCtx, "http "+call.Path)
	defer httpSpan.End()
	req = req.WithContext(httpCtx)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		httpSpan.RecordError(err)
		httpSpan.SetStatus(codes.Error, "transport_error")
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, s.config.MaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errUnreadableResponse, err)
	}
	log.Infof(ctx, "%s %s -> %s, %d bytes", method, requested, resp.Status, len(body))
	if int64(len(body)) > s.config.MaxResponseBytes {
		// Asking again will not make the answer smaller.
		return nil, util.DoNotRetry(fmt.Errorf("%w: it exceeds the %d byte limit",
			errUnreadableResponse, s.config.MaxResponseBytes))
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
			resp.Status, method, requested, s.explainRedacted(body))
		httpSpan.SetStatus(codes.Error, resp.Status)

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

		err := &upstreamStatusError{statusCode: resp.StatusCode, status: resp.Status}
		// 5xx and 429 ask to be tried again. Every other 4xx is a statement
		// about the request, which will not improve.
		if resp.StatusCode < http.StatusInternalServerError && resp.StatusCode != http.StatusTooManyRequests {
			return nil, util.DoNotRetry(err)
		}
		return nil, err
	}
	return body, nil
}
