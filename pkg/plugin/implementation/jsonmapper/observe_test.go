package jsonmapper

import (
	"context"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// These install the global providers, so none of them may run in parallel.

func recordMappingSpans(t *testing.T) func() tracetest.SpanStubs {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter)))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})
	return exporter.GetSpans
}

func recordMappingMetrics(t *testing.T) func() metricdata.ResourceMetrics {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetMeterProvider(previous)
	})
	return func() metricdata.ResourceMetrics {
		var collected metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &collected); err != nil {
			t.Fatalf("collecting metrics: %v", err)
		}
		return collected
	}
}

// loadsBySource counts the histogram's observations carrying one source label.
func loadsBySource(t *testing.T, collected metricdata.ResourceMetrics, source string) uint64 {
	t.Helper()
	var total uint64
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "oan_mapping_load_duration_seconds" {
				continue
			}
			histogram, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("%s is %T, want a float64 histogram", m.Name, m.Data)
			}
			for _, point := range histogram.DataPoints {
				if value, present := point.Attributes.Value(attrMappingSource); present &&
					value.AsString() == source {
					total += point.Count
				}
			}
		}
	}
	return total
}

// A mapping reference is a URL, so the first request after a restart or a TTL
// lapse fetches it. That is expected once per cacheTTL -- and the same rate on
// every request means the cache is not working. The source label is what tells
// those apart; the two debug lines that used to be the only evidence carried
// second-resolution timestamps and could not even be subtracted.
func TestAMappingLoadRecordsWhetherItWasFetchedOrCached(t *testing.T) {
	collect := recordMappingMetrics(t)

	var fetches atomic.Int32
	server := newMappingServer(t, bothDirections, &fetches)
	defer server.Close()

	mapper := newTestMapper(t)
	ctx := t.Context()

	// First: nothing cached, so this fetches and compiles.
	if _, err := mapper.Transform(ctx, server.URL, "request", requestInput()); err != nil {
		t.Fatalf("first transform: %v", err)
	}
	// Second: the compiled mapping is cached, and the origin is not asked again.
	if _, err := mapper.Transform(ctx, server.URL, "request", requestInput()); err != nil {
		t.Fatalf("second transform: %v", err)
	}
	if got := fetches.Load(); got != 1 {
		t.Fatalf("the mapping host was asked %d times, want 1", got)
	}

	collected := collect()
	if got := loadsBySource(t, collected, mappingLoaded); got != 1 {
		t.Errorf("loads{source=load} = %d, want 1", got)
	}
	if got := loadsBySource(t, collected, mappingCached); got != 1 {
		t.Errorf("loads{source=cache} = %d, want 1", got)
	}
}

// The span puts the fetch inside the request that paid for it, which is what
// separates "the mapping host was slow" from "the provider was slow" in a trace.
func TestAMappingLoadOpensASpan(t *testing.T) {
	spans := recordMappingSpans(t)

	server := newMappingServer(t, bothDirections, nil)
	defer server.Close()

	mapper := newTestMapper(t)
	if _, err := mapper.Transform(t.Context(), server.URL, "request", requestInput()); err != nil {
		t.Fatalf("transform: %v", err)
	}

	var found bool
	for _, span := range spans() {
		if span.Name != "mapping load" {
			continue
		}
		found = true
		var source string
		for _, kv := range span.Attributes {
			if kv.Key == attrMappingSource {
				source = kv.Value.AsString()
			}
		}
		if source != mappingLoaded {
			t.Errorf("oan.mapping.source = %q, want %q", source, mappingLoaded)
		}
	}
	if !found {
		t.Error("no \"mapping load\" span was produced")
	}
}

// A reference that cannot be fetched is the case worth timing: it is bounded by
// fetchTimeout, so a mapping host that accepts the connection and goes quiet
// costs every request that budget before anything else happens.
func TestAFailedMappingLoadIsStillRecorded(t *testing.T) {
	collect := recordMappingMetrics(t)

	mapper := newTestMapper(t)
	// A reference nothing serves.
	if _, err := mapper.Transform(t.Context(), "http://127.0.0.1:1/never", "request", requestInput()); err == nil {
		t.Fatal("expected an unreachable mapping reference to fail")
	}

	if got := loadsBySource(t, collect(), mappingLoaded); got == 0 {
		t.Error("a failed load recorded nothing; a dead mapping host would be invisible in metrics")
	}
}
