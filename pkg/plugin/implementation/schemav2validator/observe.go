// What a schema load cost, and whether it was paid on the network.
//
// This is the hop nothing measured. A capability pack's @context is FETCHED
// per payload until it caches, and one pack pulls 13-16 documents; on a cold
// cache that dominated a real request end to end while the provider call
// inside it returned in milliseconds. The provider was instrumented and the
// thing actually costing the time was not.
//
// The label that matters is `source`: a slow network load is expected once per
// TTL, and a graph that cannot separate "fetched" from "cache hit" cannot tell
// a cold start from a cache that has stopped working.
package schemav2validator

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

// Where a schema came from. A small closed set: these are metric labels.
const (
	sourceMemory  = "memory"  // preloaded rawSchemas, localSchema mode
	sourceLRU     = "lru"     // previously parsed, still within TTL
	sourceNetwork = "network" // fetched over http(s) -- the expensive one
	sourceFile    = "file"    // read from disk
	sourceUnknown = "unknown" // returned before the chain resolved
)

const (
	attrSchemaSource = attribute.Key("oan.schema.source")
	attrSchemaPath   = attribute.Key("oan.schema.path")
	attrSchemaOK     = attribute.Key("oan.schema.ok")
)

// Rebuilt when the global meter provider is replaced, so instruments created
// against the no-op provider at startup do not outlive it.
var schemaInstruments struct {
	mu       sync.RWMutex
	provider metric.MeterProvider
	loads    metric.Float64Histogram
}

func schemaLoadHistogram() metric.Float64Histogram {
	current := otel.GetMeterProvider()

	schemaInstruments.mu.RLock()
	if schemaInstruments.provider == current && schemaInstruments.loads != nil {
		h := schemaInstruments.loads
		schemaInstruments.mu.RUnlock()
		return h
	}
	schemaInstruments.mu.RUnlock()

	schemaInstruments.mu.Lock()
	defer schemaInstruments.mu.Unlock()
	if schemaInstruments.provider == current && schemaInstruments.loads != nil {
		return schemaInstruments.loads
	}

	meter := current.Meter(telemetry.ScopeName,
		metric.WithInstrumentationVersion(telemetry.ScopeVersion))
	// Buckets reach 30s: the download timeout is configurable up there, and a
	// histogram topping out at 1s reports every slow fetch identically.
	h, err := meter.Float64Histogram(
		"oan_schema_load_duration_seconds",
		metric.WithDescription("Time to resolve one schema document, by where it came from"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 2.5, 5, 10, 30),
	)
	if err != nil {
		// Deliberately silent and nil. A meter that will not build is an
		// operator problem, and this runs per schema document -- logging here
		// would log 16 times per cold payload.
		return nil
	}

	schemaInstruments.provider = current
	schemaInstruments.loads = h
	return h
}

// schemaLoad is one in-flight resolution: its span, its clock, and where the
// document turned out to come from.
type schemaLoad struct {
	span    trace.Span
	started time.Time
	path    string

	// Set by the resolution chain as it discovers which step answered. Starts
	// unknown so a path that returns before deciding is not silently counted
	// as a cache hit.
	source string
}

// startSchemaLoad opens the span and starts the clock. The returned context
// carries the span, so the fetch inside it nests correctly.
func startSchemaLoad(ctx context.Context, path string) (context.Context, *schemaLoad) {
	tracer := otel.Tracer(telemetry.ScopeName,
		trace.WithInstrumentationVersion(telemetry.ScopeVersion))
	ctx, span := tracer.Start(ctx, "schema load",
		trace.WithAttributes(attrSchemaPath.String(path)))
	return ctx, &schemaLoad{span: span, started: time.Now(), path: path, source: sourceUnknown}
}

// done closes the span and records the load. Safe to call with a nil
// histogram: telemetry must never be able to fail a validation.
func (l *schemaLoad) done(ctx context.Context, err error) {
	elapsed := time.Since(l.started)

	l.span.SetAttributes(
		attrSchemaSource.String(l.source),
		attrSchemaOK.Bool(err == nil),
	)
	if err != nil {
		// The message, not the error: a load failure quotes the URL back, and
		// that URL came from the payload.
		l.span.SetStatus(codes.Error, "schema load failed")
	}
	l.span.End()

	if h := schemaLoadHistogram(); h != nil {
		h.Record(ctx, elapsed.Seconds(), metric.WithAttributes(
			attrSchemaSource.String(l.source),
			attrSchemaOK.Bool(err == nil),
		))
	}
}
