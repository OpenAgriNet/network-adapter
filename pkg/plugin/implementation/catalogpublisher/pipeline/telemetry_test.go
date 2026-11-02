package pipeline

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// withTelemetry points the global OTel providers at in-memory ones for one
// test, and puts the previous ones back afterwards.
func withTelemetry(t *testing.T) (*sdkmetric.ManualReader, *tracetest.SpanRecorder) {
	t.Helper()
	previousMeter, previousTracer := otel.GetMeterProvider(), otel.GetTracerProvider()
	reader := sdkmetric.NewManualReader()
	recorder := tracetest.NewSpanRecorder()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	t.Cleanup(func() {
		otel.SetMeterProvider(previousMeter)
		otel.SetTracerProvider(previousTracer)
	})
	return reader, recorder
}

// counted sums a counter's points whose attributes include every one of want.
func counted(t *testing.T, reader *sdkmetric.ManualReader, name string, want ...attribute.KeyValue) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var total int64
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want an int64 counter", name, m.Data)
			}
			for _, point := range sum.DataPoints {
				matches := true
				for _, kv := range want {
					if got, ok := point.Attributes.Value(kv.Key); !ok || got != kv.Value {
						matches = false
					}
				}
				if matches {
					total += point.Value
				}
			}
		}
	}
	return total
}

// A run that does its work is one span, and counts as a run with its
// outcome; every upstream call it made is counted by path and outcome.
func TestARunIsOneSpanAndCountsItsOutcome(t *testing.T) {
	reader, recorder := withTelemetry(t)
	upstream := newFixtureUpstream(t, twoGroupsOfThings)

	report, err := Run(context.Background(), RunOptions{
		Pipeline: fixturePipeline(), Record: publishingRecord(), RunLog: &fakeRunLog{},
		Lookup: fixtureEnv(upstream.URL), Now: firedAt(t), OutDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	spans := recorder.Ended()
	if len(spans) != 1 || spans[0].Name() != "publish pipeline run" {
		t.Fatalf("spans = %d, want one \"publish pipeline run\"", len(spans))
	}
	attrs := map[attribute.Key]attribute.Value{}
	for _, kv := range spans[0].Attributes() {
		attrs[kv.Key] = kv.Value
	}
	if attrs["capability"].AsString() != report.Capability || attrs["outcome"].AsString() != "built" ||
		attrs["catalogs"].AsInt64() != int64(len(report.Catalogs)) {
		t.Errorf("span attributes = %v; want capability %q, outcome built, catalogs %d",
			attrs, report.Capability, len(report.Catalogs))
	}

	if got := counted(t, reader, "onix_publish_runs_total",
		attribute.String("capability", report.Capability), attribute.String("outcome", "built")); got != 1 {
		t.Errorf("onix_publish_runs_total{outcome=built} = %d, want 1", got)
	}
	if got := counted(t, reader, "onix_publish_upstream_calls_total", attribute.String("outcome", "ok")); got == 0 {
		t.Error("no successful upstream call was counted")
	}
}

// A tick that finds nothing due is counted, but makes no span: it happens
// thousands of times a day and does no work worth a trace.
func TestANotDueTickIsCountedButMakesNoSpan(t *testing.T) {
	reader, recorder := withTelemetry(t)
	upstream := newFixtureUpstream(t, twoGroupsOfThings)
	ran := firedAt(t)

	report, err := Run(context.Background(), RunOptions{
		Pipeline: fixturePipeline(), Record: publishingRecord(), RunLog: &fakeRunLog{last: ran},
		Lookup: fixtureEnv(upstream.URL), Now: ran.Add(5 * time.Minute),
	})
	if err != nil || report.Due {
		t.Fatalf("Run = %+v, %v; want a not-due tick", report, err)
	}
	if spans := recorder.Ended(); len(spans) != 0 {
		t.Errorf("a not-due tick made %d spans, want none", len(spans))
	}
	if got := counted(t, reader, "onix_publish_runs_total", attribute.String("outcome", "notDue")); got != 1 {
		t.Errorf("onix_publish_runs_total{outcome=notDue} = %d, want 1", got)
	}
}

// A run that fails is a failed span and a failed run.
func TestAFailedRunIsAFailedSpan(t *testing.T) {
	reader, recorder := withTelemetry(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"token":"tok"}`))
			return
		}
		http.Error(w, `{"error":"down"}`, http.StatusInternalServerError)
	}))
	defer upstream.Close()

	if _, err := Run(context.Background(), RunOptions{
		Pipeline: fixturePipeline(), Record: publishingRecord(), RunLog: &fakeRunLog{},
		Lookup: fixtureEnv(upstream.URL), Now: firedAt(t), OutDir: t.TempDir(),
	}); err == nil {
		t.Fatal("a run against a failing upstream succeeded")
	}
	spans := recorder.Ended()
	if len(spans) != 1 || spans[0].Status().Code != codes.Error {
		t.Fatalf("spans = %v, want one with status Error", spans)
	}
	if got := counted(t, reader, "onix_publish_runs_total", attribute.String("outcome", "failed")); got != 1 {
		t.Errorf("onix_publish_runs_total{outcome=failed} = %d, want 1", got)
	}
	if got := counted(t, reader, "onix_publish_upstream_calls_total", attribute.String("outcome", "error")); got == 0 {
		t.Error("a failed upstream call was not counted")
	}
}
