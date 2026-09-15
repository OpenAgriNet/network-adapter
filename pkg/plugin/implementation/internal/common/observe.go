// What a provider call cost, as a span and as metrics.
//
// Every capability -- AgricultureFacility, KnowledgeAdvisory, MandiPrice,
// WeatherObservation -- reaches its provider through serve.go's call(), so
// instrumenting that one path instruments all of them. Nothing here is
// capability-specific, and nothing here belongs in a capability package.
//
// The three answers this file exists to give, in the order an operator asks
// them:
//
//	how long did THIS call take        the duration on the log line
//	where did the time go              the span, sitting beside the mapping
//	                                   fetch inside the step's own span
//	is this provider getting worse     the histogram and the counter
//
// The log line answers one request. The span answers one trace. The metrics
// answer a thousand requests, which is the only one of the three that can
// describe a trend.
package common

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Attribute keys. Span and metric attributes are deliberately NOT the same
// set: a span describes one call and can afford the full URL, while a metric
// label multiplies series and cannot. url and attempt are span-only for that
// reason -- a query string with a date range in it would mint a new time
// series per request.
const (
	attrProvider    = attribute.Key("oan.provider")
	attrCapability  = attribute.Key("oan.capability")
	attrOutcome     = attribute.Key("oan.outcome")
	attrAttempt     = attribute.Key("oan.attempt")
	attrMethod      = attribute.Key("http.request.method")
	attrURL         = attribute.Key("url.full")
	attrStatusCode  = attribute.Key("http.response.status_code")
	attrStatusClass = attribute.Key("http.response.status_class")
	attrBodyBytes   = attribute.Key("http.response.body.size")
)

// Outcomes. Coarse on purpose: these are metric label values, so the set has
// to be small and closed. The detail belongs on the span and in the log.
const (
	outcomeOK         = "ok"
	outcomeClientErr  = "http_4xx"
	outcomeServerErr  = "http_5xx"
	outcomeTimeout    = "timeout"
	outcomeCanceled   = "canceled"
	outcomeCredential = "credential"
	outcomeTransport  = "transport"
	// The provider answered, and we refused what it said -- an oversized body
	// is the case today. Worth its own value: it is neither the provider
	// failing nor the call succeeding, and lumping it under either would put
	// a limit of ours on somebody else's error rate.
	outcomeRejected = "rejected"
)

// bindingCtxKey carries the binding key from serve() down to the call layer.
//
// A context value rather than two more parameters on call() and attempt():
// those signatures are exercised by the existing tests, and the binding is
// ambient request identity -- exactly what transaction_id and module_id
// already travel as.
type bindingCtxKey struct{}

// withBinding records which provider capability this request is serving.
func withBinding(ctx context.Context, bindingKey string) context.Context {
	return context.WithValue(ctx, bindingCtxKey{}, bindingKey)
}

// bindingFrom returns the provider and capability halves of the binding key,
// or two empty strings when the context carries none. Empty rather than an
// error: telemetry that fails is worse than telemetry that is unlabelled, and
// nothing here should be able to fail a request.
func bindingFrom(ctx context.Context) (provider, capability string) {
	key, _ := ctx.Value(bindingCtxKey{}).(string)
	if key == "" {
		return "", ""
	}
	provider, capability, _ = strings.Cut(key, separator)
	return strings.TrimSpace(provider), strings.TrimSpace(capability)
}

// instruments holds what this package records. Built once per meter provider.
type instruments struct {
	duration metric.Float64Histogram
	calls    metric.Int64Counter
}

// Rebuilt when the global provider is replaced, matching
// telemetry.GetMetrics. Without this a test that installs its own reader --
// or otelsetup registering the real provider after start -- would keep
// recording into instruments bound to the no-op provider it replaced.
var instrumentCache struct {
	mu       sync.RWMutex
	provider metric.MeterProvider
	i        *instruments
}

func getInstruments() (*instruments, error) {
	current := otel.GetMeterProvider()

	instrumentCache.mu.RLock()
	if instrumentCache.provider == current && instrumentCache.i != nil {
		i := instrumentCache.i
		instrumentCache.mu.RUnlock()
		return i, nil
	}
	instrumentCache.mu.RUnlock()

	instrumentCache.mu.Lock()
	defer instrumentCache.mu.Unlock()
	if instrumentCache.provider == current && instrumentCache.i != nil {
		return instrumentCache.i, nil
	}

	meter := current.Meter(telemetry.ScopeName,
		metric.WithInstrumentationVersion(telemetry.ScopeVersion))

	i := &instruments{}
	var err error
	// Buckets reach 30s because a provider timeout ceiling lives up there and
	// a histogram whose top bucket is 1s reports every timeout identically.
	if i.duration, err = meter.Float64Histogram(
		"oan_provider_call_duration_seconds",
		metric.WithDescription("Time spent on one call to a provider's backend, including the response read"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30),
	); err != nil {
		return nil, fmt.Errorf("oan_provider_call_duration_seconds: %w", err)
	}
	if i.calls, err = meter.Int64Counter(
		"oan_provider_calls_total",
		metric.WithDescription("Calls to a provider's backend, by outcome"),
		metric.WithUnit("{call}"),
	); err != nil {
		return nil, fmt.Errorf("oan_provider_calls_total: %w", err)
	}

	instrumentCache.provider = current
	instrumentCache.i = i
	return i, nil
}

// providerCall is one in-flight attempt: its span, its clock, and the labels
// both the span and the metrics carry.
type providerCall struct {
	span       trace.Span
	started    time.Time
	method     string
	provider   string
	capability string
}

// startProviderCall opens the span for one attempt and starts its clock.
//
// The span is a CHILD of the step's span rather than the step's span itself.
// That is the whole point: today the provider call shares a span with the
// mapping fetch from GitHub, so a slow request cannot be attributed to either
// one. A retried call also becomes several sibling spans instead of one long
// opaque one.
//
// url is the redacted string -- the caller has already removed the credential,
// and a span export is no safer a place for a token than a log line.
func startProviderCall(ctx context.Context, method, url string, attempt, attempts int) (context.Context, *providerCall) {
	provider, capability := bindingFrom(ctx)

	tracer := otel.Tracer(telemetry.ScopeName,
		trace.WithInstrumentationVersion(telemetry.ScopeVersion))
	ctx, span := tracer.Start(ctx, "provider call",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attrMethod.String(method),
			attrURL.String(url),
			attrProvider.String(provider),
			attrCapability.String(capability),
			attrAttempt.Int(attempt),
			attribute.Int("oan.attempts_allowed", attempts),
		))

	return ctx, &providerCall{
		span:       span,
		started:    time.Now(),
		method:     method,
		provider:   provider,
		capability: capability,
	}
}

// setURL replaces the URL the span was opened with.
//
// The span has to exist before the credential is attached -- that is where an
// unset environment variable fails -- but the URL is only safe to record
// afterwards, once redactString has been over it. So it is set twice: the
// pre-credential endpoint at the start, the redacted wire URL here. The later
// value wins, and a call that never got a credential keeps the earlier one,
// which carries nothing secret because nothing secret had been added yet.
func (c *providerCall) setURL(url string) {
	c.span.SetAttributes(attrURL.String(url))
}

// elapsed is how long the attempt has taken so far. Read by the caller for the
// log line, so the log and the metrics report one measurement rather than two
// that disagree.
func (c *providerCall) elapsed() time.Duration { return time.Since(c.started) }

// done closes the span and records the metrics.
//
// status is 0 when the request never produced a response. err being nil is not
// the same as a 2xx: a 404 is a completed call that failed, and both the span
// and the counter have to say so.
func (c *providerCall) done(ctx context.Context, status, bodyBytes int, err error) {
	elapsed := c.elapsed()
	outcome := classify(status, err)

	c.span.SetAttributes(
		attrStatusCode.Int(status),
		attrBodyBytes.Int(bodyBytes),
		attrOutcome.String(outcome),
	)
	if outcome == outcomeOK {
		c.span.SetStatus(codes.Ok, "")
	} else {
		// The outcome, not the error: RecordError would put a provider's
		// rejection body on the span, and those quote the request back
		// credential and all.
		c.span.SetStatus(codes.Error, outcome)
	}
	c.span.End()

	i, instrErr := getInstruments()
	if instrErr != nil {
		// Deliberately silent. A meter that will not build is an operator
		// problem, and logging it here would log it once per provider call.
		return
	}
	attrs := metric.WithAttributes(
		attrProvider.String(c.provider),
		attrCapability.String(c.capability),
		attrMethod.String(c.method),
		attrOutcome.String(outcome),
		attrStatusClass.String(statusClass(status)),
	)
	i.duration.Record(ctx, elapsed.Seconds(), attrs)
	i.calls.Add(ctx, 1, attrs)
}

// classify reduces an outcome to one of a small closed set, because these are
// metric label values.
//
// Order matters. A cancelled context surfaces as a transport error with a
// deadline inside it, so the error is examined before the status code.
func classify(status int, err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return outcomeTimeout
	case errors.Is(err, context.Canceled):
		return outcomeCanceled
	case err != nil && status == 0:
		// No response at all. A credential that could not be read never
		// reached the wire either, and is worth telling apart from a provider
		// that is down -- one is our configuration, the other is weather.
		if isCredentialErr(err) {
			return outcomeCredential
		}
		return outcomeTransport
	case status >= http.StatusInternalServerError:
		return outcomeServerErr
	case status >= http.StatusBadRequest:
		return outcomeClientErr
	case err != nil:
		// A 2xx we could not use. The provider did its part.
		return outcomeRejected
	}
	return outcomeOK
}

// isCredentialErr reports whether the call failed before the wire because a
// credential could not be assembled. missingCredential words it this way.
func isCredentialErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "is not configured")
}

// statusClass buckets a status into 2xx/4xx/5xx, keeping the label's
// cardinality at a handful rather than one series per status code.
func statusClass(status int) string {
	if status == 0 {
		return "none"
	}
	return fmt.Sprintf("%dxx", status/100)
}
