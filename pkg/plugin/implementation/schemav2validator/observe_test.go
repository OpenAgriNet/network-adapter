package schemav2validator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// These install the global providers, so none of them may run in parallel.

const probeSchema = `openapi: 3.0.0
info:
  title: Probe
  version: 1.0.0
components:
  schemas:
    Probe:
      type: object`

func recordSchemaSpans(t *testing.T) func() tracetest.SpanStubs {
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

func recordSchemaMetrics(t *testing.T) func() metricdata.ResourceMetrics {
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

// loadsBySource counts the histogram's observations for one source label.
func loadsBySource(t *testing.T, collected metricdata.ResourceMetrics, source string) uint64 {
	t.Helper()
	var total uint64
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "oan_schema_load_duration_seconds" {
				continue
			}
			histogram, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("%s is %T, want a float64 histogram", m.Name, m.Data)
			}
			for _, point := range histogram.DataPoints {
				if value, present := point.Attributes.Value(attrSchemaSource); present &&
					value.AsString() == source {
					total += point.Count
				}
			}
		}
	}
	return total
}

// THE WHOLE POINT OF THE source LABEL. A network load is expected once per TTL;
// the same rate on every request means the cache has stopped working. Without
// the label those two are the same graph, which is why a real request spent two
// seconds here and nothing said so.
func TestASchemaLoadRecordsWhetherItWasFetchedOrCached(t *testing.T) {
	collect := recordSchemaMetrics(t)

	var served int
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served++
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write([]byte(probeSchema))
	}))
	defer origin.Close()

	cache := newSchemaCache(10)
	ctx := t.Context()
	url := origin.URL + "/Probe/attributes.yaml"

	// First load: nothing cached, so it goes to the network.
	if _, err := cache.loadSchemaFromPath(ctx, url, time.Hour, 30*time.Second, nil, false); err != nil {
		t.Fatalf("first load: %v", err)
	}
	// Second: the LRU answers, and the origin must not be asked again.
	if _, err := cache.loadSchemaFromPath(ctx, url, time.Hour, 30*time.Second, nil, false); err != nil {
		t.Fatalf("second load: %v", err)
	}
	if served != 1 {
		t.Fatalf("the origin was asked %d times, want 1 -- the second load should have been cached", served)
	}

	collected := collect()
	if got := loadsBySource(t, collected, sourceNetwork); got != 1 {
		t.Errorf("loads{source=network} = %d, want 1", got)
	}
	if got := loadsBySource(t, collected, sourceLRU); got != 1 {
		t.Errorf("loads{source=lru} = %d, want 1", got)
	}
}

// The span is what puts the fetch under the validation step in a trace, which
// is how a slow request gets attributed to the schema rather than the provider.
func TestASchemaLoadOpensASpanNamingWhereItCameFrom(t *testing.T) {
	spans := recordSchemaSpans(t)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write([]byte(probeSchema))
	}))
	defer origin.Close()

	cache := newSchemaCache(10)
	url := origin.URL + "/Probe/attributes.yaml"
	if _, err := cache.loadSchemaFromPath(t.Context(), url, time.Hour, 30*time.Second, nil, false); err != nil {
		t.Fatalf("load: %v", err)
	}

	var found bool
	for _, span := range spans() {
		if span.Name != "schema load" {
			continue
		}
		found = true
		source, present := span.Attributes[0], false
		for _, kv := range span.Attributes {
			if kv.Key == attrSchemaSource {
				source, present = kv, true
			}
		}
		if !present {
			t.Fatalf("the span carries no %s; it has %v", attrSchemaSource, span.Attributes)
		}
		if source.Value.AsString() != sourceNetwork {
			t.Errorf("oan.schema.source = %q, want %q", source.Value.AsString(), sourceNetwork)
		}
	}
	if !found {
		t.Error("no \"schema load\" span was produced")
	}
}

// A failure is still a load, and it is the one an operator most wants timed:
// a schema host that hangs holds every payload for the download timeout.
func TestAFailedSchemaLoadIsStillRecorded(t *testing.T) {
	collect := recordSchemaMetrics(t)

	cache := newSchemaCache(10)
	// localSchema=false refuses a non-http path, so this fails without a
	// network round trip.
	if _, err := cache.loadSchemaFromPath(t.Context(), "/nonexistent/schema.yaml",
		time.Hour, 30*time.Second, nil, false); err == nil {
		t.Fatal("expected a payload-directed local path to be refused")
	}

	var any uint64
	for _, source := range []string{sourceNetwork, sourceFile, sourceLRU, sourceMemory, sourceUnknown} {
		any += loadsBySource(t, collect(), source)
	}
	if any == 0 {
		t.Error("a failed load recorded nothing; a hanging schema host would be invisible")
	}
}
