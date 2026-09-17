// What resolving a mapping cost, and whether it was paid on the network.
//
// A mapping reference is a URL the registry publishes, so the first request
// after a restart or a TTL lapse fetches it over HTTP and compiles it. Nothing
// recorded that: the only evidence was two debug lines with second-resolution
// timestamps, which cannot be subtracted.
//
// The `source` label is the point. A load is expected once per cacheTTL; the
// same graph showing loads on every request means the cache is not working,
// and without the label those two look identical.
package jsonmapper

import (
	"context"
	"sync"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Where a compiled mapping came from. A small closed set: metric labels.
const (
	mappingCached = "cache" // still within cacheTTL, nothing fetched
	mappingLoaded = "load"  // went through fetch and compile, or waited on one
)

const (
	attrMappingSource = attribute.Key("oan.mapping.source")
	attrMappingRef    = attribute.Key("oan.mapping.ref")
	attrMappingOK     = attribute.Key("oan.mapping.ok")
)

// Rebuilt when the global meter provider is replaced, so an instrument bound
// to the no-op provider at startup does not outlive it.
var mappingInstruments struct {
	mu       sync.RWMutex
	provider metric.MeterProvider
	loads    metric.Float64Histogram
}

func mappingLoadHistogram() metric.Float64Histogram {
	current := otel.GetMeterProvider()

	mappingInstruments.mu.RLock()
	if mappingInstruments.provider == current && mappingInstruments.loads != nil {
		h := mappingInstruments.loads
		mappingInstruments.mu.RUnlock()
		return h
	}
	mappingInstruments.mu.RUnlock()

	mappingInstruments.mu.Lock()
	defer mappingInstruments.mu.Unlock()
	if mappingInstruments.provider == current && mappingInstruments.loads != nil {
		return mappingInstruments.loads
	}

	meter := current.Meter(telemetry.ScopeName,
		metric.WithInstrumentationVersion(telemetry.ScopeVersion))
	// The bottom buckets are deliberately fine: a cache hit should land in
	// microseconds, and a hit that has crept into milliseconds is worth seeing
	// before it becomes a fetch-shaped latency nobody can explain.
	h, err := meter.Float64Histogram(
		"oan_mapping_load_duration_seconds",
		metric.WithDescription("Time to resolve one mapping reference, by whether it was cached"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.0001, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 2.5, 5, 10),
	)
	if err != nil {
		// Silent and nil on purpose: this runs on every mapped request, so a
		// log here would be a log per request.
		return nil
	}

	mappingInstruments.provider = current
	mappingInstruments.loads = h
	return h
}

// mappingLoad is one in-flight resolution: its span, its clock, and whether it
// turned out to be cached.
type mappingLoad struct {
	span    trace.Span
	started time.Time

	// Assumed cached, corrected to loaded by whichever path pays. The
	// optimistic default is the safe one: mislabelling a cheap hit as a load
	// would invent network traffic that never happened.
	source string
}

// startMappingLoad opens the span and starts the clock. The returned context
// carries the span so the fetch nests under it.
func startMappingLoad(ctx context.Context, ref string) (context.Context, *mappingLoad) {
	tracer := otel.Tracer(telemetry.ScopeName,
		trace.WithInstrumentationVersion(telemetry.ScopeVersion))
	ctx, span := tracer.Start(ctx, "mapping load",
		trace.WithAttributes(attrMappingRef.String(ref)))
	return ctx, &mappingLoad{span: span, started: time.Now(), source: mappingCached}
}

// done closes the span and records the load. Safe with a nil histogram:
// telemetry must never be able to fail a mapping.
func (l *mappingLoad) done(ctx context.Context, err error) {
	elapsed := time.Since(l.started)

	l.span.SetAttributes(
		attrMappingSource.String(l.source),
		attrMappingOK.Bool(err == nil),
	)
	if err != nil {
		// The status, not the error: a compile failure quotes the mapping
		// back, and a mapping is somebody's published document.
		l.span.SetStatus(codes.Error, "mapping load failed")
	}
	l.span.End()

	if h := mappingLoadHistogram(); h != nil {
		h.Record(ctx, elapsed.Seconds(), metric.WithAttributes(
			attrMappingSource.String(l.source),
			attrMappingOK.Bool(err == nil),
		))
	}
}
