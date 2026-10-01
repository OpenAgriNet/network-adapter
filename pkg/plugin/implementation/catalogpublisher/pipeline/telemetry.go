package pipeline

// telemetry.go is a run's span and its metrics.
//
// A run that does its work is ONE span, "publish pipeline run", with its
// capability, outcome and counts. A tick that finds nothing due makes no span:
// it happens thousands of times a day and does nothing worth a trace. It is
// still counted, so "is the sweep alive" is answerable from metrics alone.
//
// The upstream calls inside a run are counted, not traced: a Direct run makes
// thousands, and a span each would bury the one span that matters.

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/beckn-one/beckn-onix/pkg/telemetry"
)

// Run outcomes, as onix_publish_runs_total's `outcome` attribute.
const (
	outcomeFailed           = "failed"
	outcomeNotDue           = "notDue"
	outcomeClaimedElsewhere = "claimedElsewhere"
	outcomeDryRun           = "dryRun"
	outcomeBuilt            = "built"
	outcomePublished        = "published"
)

// Upstream call outcomes, as onix_publish_upstream_calls_total's `outcome`.
const (
	callOK    = "ok"
	callEmpty = "empty"
	callError = "error"
)

var (
	attrCapability = attribute.Key("capability")
	attrOutcome    = attribute.Key("outcome")
	attrStatus     = attribute.Key("status")
	attrPath       = attribute.Key("path")
	attrCatalogs   = attribute.Key("catalogs")
	attrPublished  = attribute.Key("published")
)

type publishInstruments struct {
	runs     metric.Int64Counter
	duration metric.Float64Histogram
	catalogs metric.Int64Counter
	calls    metric.Int64Counter
}

// instruments are bound to the global MeterProvider, and rebound only when
// otel.SetMeterProvider replaces it -- the same rule telemetry.GetMetrics
// follows for the plugin metrics.
var instruments struct {
	mu       sync.Mutex
	provider metric.MeterProvider
	set      *publishInstruments
}

// meters returns the instruments, or nil when they cannot be built: a run
// never fails over its own telemetry.
func meters() *publishInstruments {
	current := otel.GetMeterProvider()
	instruments.mu.Lock()
	defer instruments.mu.Unlock()
	if instruments.provider == current && instruments.set != nil {
		return instruments.set
	}
	meter := current.Meter(telemetry.ScopeName, metric.WithInstrumentationVersion(telemetry.ScopeVersion))
	set := &publishInstruments{}
	var err error
	if set.runs, err = meter.Int64Counter("onix_publish_runs_total",
		metric.WithDescription("Publish pipeline ticks, by outcome"), metric.WithUnit("{run}")); err != nil {
		return nil
	}
	if set.duration, err = meter.Float64Histogram("onix_publish_run_duration_seconds",
		metric.WithDescription("Time a publish pipeline run spent doing its work"), metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(1, 5, 15, 60, 300, 900, 1800, 3600, 7200, 14400)); err != nil {
		return nil
	}
	if set.catalogs, err = meter.Int64Counter("onix_publish_catalogs_total",
		metric.WithDescription("Catalogs a publish pipeline sent, by result"), metric.WithUnit("{catalog}")); err != nil {
		return nil
	}
	if set.calls, err = meter.Int64Counter("onix_publish_upstream_calls_total",
		metric.WithDescription("Upstream calls a publish pipeline made, by path and outcome"), metric.WithUnit("{call}")); err != nil {
		return nil
	}
	instruments.provider, instruments.set = current, set
	return set
}

// runOutcome names what a tick came to.
func runOutcome(report RunReport, dryRun bool, err error) string {
	switch {
	case err != nil:
		return outcomeFailed
	case !report.Due:
		return outcomeNotDue
	case report.ClaimedElsewhere:
		return outcomeClaimedElsewhere
	case dryRun:
		return outcomeDryRun
	case report.Published != nil:
		return outcomePublished
	default:
		return outcomeBuilt
	}
}

// recordTick counts one tick, and the catalogs it sent.
func recordTick(ctx context.Context, report RunReport, dryRun bool, err error) {
	m := meters()
	if m == nil {
		return
	}
	capability := attrCapability.String(report.Capability)
	m.runs.Add(ctx, 1, metric.WithAttributes(capability, attrOutcome.String(runOutcome(report, dryRun, err))))
	if report.Published == nil {
		return
	}
	for _, outcome := range report.Published.Outcomes {
		m.catalogs.Add(ctx, 1, metric.WithAttributes(capability, attrStatus.String(string(outcome.Status))))
	}
}

// startRunSpan opens the one span a run that does its work gets.
func startRunSpan(ctx context.Context, capability string) (context.Context, trace.Span) {
	tracer := otel.Tracer(telemetry.ScopeName, trace.WithInstrumentationVersion(telemetry.ScopeVersion))
	return tracer.Start(ctx, "publish pipeline run", trace.WithAttributes(attrCapability.String(capability)))
}

// finishRunSpan records what the run came to on its span, and how long the
// work took.
func finishRunSpan(ctx context.Context, span trace.Span, report RunReport, err error, took time.Duration) {
	published := 0
	if report.Published != nil {
		published = len(report.Published.Outcomes)
	}
	span.SetAttributes(
		attrOutcome.String(runOutcome(report, false, err)),
		attrCatalogs.Int(len(report.Catalogs)),
		attrPublished.Int(published),
	)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "publish pipeline run failed")
	}
	if m := meters(); m != nil {
		m.duration.Record(ctx, took.Seconds(), metric.WithAttributes(attrCapability.String(report.Capability)))
	}
}

// recordUpstreamCall counts one upstream call by its path -- never the query,
// which may carry the token -- and what it came to.
func recordUpstreamCall(ctx context.Context, path, outcome string) {
	if m := meters(); m != nil {
		m.calls.Add(ctx, 1, metric.WithAttributes(attrPath.String(path), attrOutcome.String(outcome)))
	}
}
