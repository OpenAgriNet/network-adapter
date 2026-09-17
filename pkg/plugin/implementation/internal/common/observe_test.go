package common

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// These tests install the global tracer and meter providers, so none of them
// can run in parallel -- with or without each other. The rest of this package
// is parallel; this file deliberately is not.

// recordSpans installs an in-memory tracer provider and returns the spans a
// test produced. The previous provider is restored, so a test here cannot
// change what the parallel tests elsewhere in the package observe.
func recordSpans(t *testing.T) func() tracetest.SpanStubs {
	t.Helper()

	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(
		sdktrace.NewSimpleSpanProcessor(exporter)))

	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})

	return exporter.GetSpans
}

// recordMetrics installs an in-memory meter provider and returns a collector.
//
// getInstruments caches on provider identity, so installing a fresh provider
// is enough to get fresh instruments -- which is the behaviour this helper
// depends on and, incidentally, tests.
func recordMetrics(t *testing.T) func() metricdata.ResourceMetrics {
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

// providerCallSpans keeps only the spans this file is about. Nothing else in
// the package opens one today, but a test that asserts on "the first span"
// would start lying the moment something did.
func providerCallSpans(spans tracetest.SpanStubs) tracetest.SpanStubs {
	var found tracetest.SpanStubs
	for _, span := range spans {
		if span.Name == "provider call" {
			found = append(found, span)
		}
	}
	return found
}

func attrOf(t *testing.T, attrs []attribute.KeyValue, key attribute.Key) attribute.Value {
	t.Helper()
	for _, kv := range attrs {
		if kv.Key == key {
			return kv.Value
		}
	}
	t.Fatalf("span carries no %s; it has %v", key, attrs)
	return attribute.Value{}
}

// sumFor returns the counter total across every data point whose attributes
// include the given outcome.
func sumFor(t *testing.T, collected metricdata.ResourceMetrics, name, outcome string) int64 {
	t.Helper()

	var total int64
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want an int64 sum", name, m.Data)
			}
			for _, point := range sum.DataPoints {
				if value, present := point.Attributes.Value(attrOutcome); present &&
					value.AsString() == outcome {
					total += point.Value
				}
			}
		}
	}
	return total
}

// histogramCount returns how many observations the named histogram recorded.
func histogramCount(t *testing.T, collected metricdata.ResourceMetrics, name string) uint64 {
	t.Helper()

	var total uint64
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			histogram, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("%s is %T, want a float64 histogram", name, m.Data)
			}
			for _, point := range histogram.DataPoints {
				total += point.Count
			}
		}
	}
	return total
}

// --- the span ---------------------------------------------------------------

// The span exists so a slow request can be attributed. Before this, the
// provider call shared the step's span with the mapping fetch from GitHub, and
// the two could not be told apart.
func TestServeOpensASpanForTheProviderCall(t *testing.T) {
	spans := recordSpans(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"fcstday1":{"rain":12.4}}`)
	}))
	defer upstream.Close()

	mapper := &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"ok":true}`)}
	step := newStep(t, &stubRegistry{plan: testPlan(upstream.URL, http.MethodGet)}, mapper)
	if _, err := runStep(t, step, selectBody); err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}

	calls := providerCallSpans(spans())
	if len(calls) != 1 {
		t.Fatalf("got %d provider call spans, want exactly 1", len(calls))
	}
	span := calls[0]

	// The labels that make the span findable: which provider, which
	// capability. Without these a trace viewer shows an anonymous HTTP call.
	if got := attrOf(t, span.Attributes, attrProvider).AsString(); got != "mausamgram" {
		t.Errorf("oan.provider = %q, want mausamgram", got)
	}
	if got := attrOf(t, span.Attributes, attrCapability).AsString(); got != "openagrinet:WeatherObservation" {
		t.Errorf("oan.capability = %q, want openagrinet:WeatherObservation", got)
	}
	if got := attrOf(t, span.Attributes, attrMethod).AsString(); got != http.MethodGet {
		t.Errorf("http.request.method = %q, want GET", got)
	}
	if got := attrOf(t, span.Attributes, attrStatusCode).AsInt64(); got != http.StatusOK {
		t.Errorf("http.response.status_code = %d, want 200", got)
	}
	if got := attrOf(t, span.Attributes, attrOutcome).AsString(); got != outcomeOK {
		t.Errorf("oan.outcome = %q, want %q", got, outcomeOK)
	}
	// The path, so the span names which endpoint was called and not merely
	// which host.
	if got := attrOf(t, span.Attributes, attrURL).AsString(); !strings.Contains(got, "/get-daily") {
		t.Errorf("url.full = %q, want the called path in it", got)
	}
	if span.Status.Code != codes.Ok {
		t.Errorf("span status = %v, want Ok", span.Status.Code)
	}
}

// A retried call becomes one span per attempt. One long span covering all of
// them would hide that the first attempt failed, and hide how much of the
// elapsed time was backoff rather than the provider.
func TestEachRetryGetsItsOwnSpan(t *testing.T) {
	spans := recordSpans(t)

	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, `{"fcstday1":{"rain":12.4}}`)
	}))
	defer upstream.Close()

	mapper := &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"ok":true}`)}
	step := newStep(t, &stubRegistry{plan: testPlan(upstream.URL, http.MethodGet)}, mapper)
	if _, err := runStep(t, step, selectBody); err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}

	recorded := providerCallSpans(spans())
	if len(recorded) != 2 {
		t.Fatalf("got %d provider call spans, want 2 -- one per attempt", len(recorded))
	}

	// The failed attempt is marked failed, and numbered so the order is
	// readable without relying on export order.
	if got := attrOf(t, recorded[0].Attributes, attrOutcome).AsString(); got != outcomeServerErr {
		t.Errorf("first attempt outcome = %q, want %q", got, outcomeServerErr)
	}
	if recorded[0].Status.Code != codes.Error {
		t.Errorf("first attempt span status = %v, want Error", recorded[0].Status.Code)
	}
	if got := attrOf(t, recorded[0].Attributes, attrAttempt).AsInt64(); got != 1 {
		t.Errorf("first attempt is numbered %d, want 1", got)
	}
	if got := attrOf(t, recorded[1].Attributes, attrOutcome).AsString(); got != outcomeOK {
		t.Errorf("second attempt outcome = %q, want %q", got, outcomeOK)
	}
	if got := attrOf(t, recorded[1].Attributes, attrAttempt).AsInt64(); got != 2 {
		t.Errorf("second attempt is numbered %d, want 2", got)
	}
}

// The span is exported. A credential on it is as leaked as one in a log line,
// and the span carries the full URL -- which for a query-credential provider
// is exactly where the token lives.
func TestTheSpanCarriesNoCredential(t *testing.T) {
	spans := recordSpans(t)

	t.Setenv("OBSERVE_TEST_TOKEN", "s3cret-value")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"fcstday1":{"rain":12.4}}`)
	}))
	defer upstream.Close()

	mapper := &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"ok":true}`)}
	step := newStep(t, &stubRegistry{plan: testPlan(upstream.URL, http.MethodGet)}, mapper,
		func(cfg *Config) {
			cfg.AuthByProvider = map[string]*AuthProfile{
				"mausamgram": {
					Scheme:        "query",
					QueryName:     "token",
					QueryValueEnv: "OBSERVE_TEST_TOKEN",
				},
			}
		})
	if _, err := runStep(t, step, selectBody); err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}

	calls := providerCallSpans(spans())
	if len(calls) != 1 {
		t.Fatalf("got %d provider call spans, want exactly 1", len(calls))
	}
	url := attrOf(t, calls[0].Attributes, attrURL).AsString()
	if strings.Contains(url, "s3cret-value") {
		t.Errorf("url.full = %q, which carries the credential", url)
	}
	// Negative control: the parameter is present, so the assertion above is
	// not passing merely because no credential was ever sent.
	if !strings.Contains(url, "token=") {
		t.Errorf("url.full = %q, want the token parameter present but redacted", url)
	}
}

// --- the metrics ------------------------------------------------------------

// Logs describe one request. This is the instrument that can answer "is this
// provider getting worse", which no amount of grep can.
func TestServeRecordsTheCallDurationAndOutcome(t *testing.T) {
	collect := recordMetrics(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"fcstday1":{"rain":12.4}}`)
	}))
	defer upstream.Close()

	mapper := &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"ok":true}`)}
	step := newStep(t, &stubRegistry{plan: testPlan(upstream.URL, http.MethodGet)}, mapper)
	if _, err := runStep(t, step, selectBody); err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}

	collected := collect()
	if got := histogramCount(t, collected, "oan_provider_call_duration_seconds"); got != 1 {
		t.Errorf("duration histogram recorded %d observations, want 1", got)
	}
	if got := sumFor(t, collected, "oan_provider_calls_total", outcomeOK); got != 1 {
		t.Errorf("calls_total{outcome=ok} = %d, want 1", got)
	}
}

// A provider that fails twice is two failures, not one request. Counting the
// request rather than the attempt would understate how hard the adapter is
// leaning on a sick provider.
func TestAFailingProviderIsCountedOnceForEachAttempt(t *testing.T) {
	collect := recordMetrics(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	mapper := &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"ok":true}`)}
	step := newStep(t, &stubRegistry{plan: testPlan(upstream.URL, http.MethodGet)}, mapper)
	if _, err := runStep(t, step, selectBody); err == nil {
		t.Fatal("Run() succeeded against a provider that only returns 500")
	}

	collected := collect()
	// testPlan allows one retry, so two attempts reach the provider.
	if got := sumFor(t, collected, "oan_provider_calls_total", outcomeServerErr); got != 2 {
		t.Errorf("calls_total{outcome=http_5xx} = %d, want 2 -- one per attempt", got)
	}
	if got := sumFor(t, collected, "oan_provider_calls_total", outcomeOK); got != 0 {
		t.Errorf("calls_total{outcome=ok} = %d, want 0", got)
	}
	if got := histogramCount(t, collected, "oan_provider_call_duration_seconds"); got != 2 {
		t.Errorf("duration histogram recorded %d observations, want 2", got)
	}
}

// The one failure an operator can actually fix, so it must not be the one
// failure that reports nothing. An unset environment variable never reaches
// the wire, so a span opened after the credential is attached would miss it
// entirely -- the request would fail with no span and no counter, looking from
// the metrics like a request that was never made.
func TestAnUnsetCredentialIsStillCounted(t *testing.T) {
	spans := recordSpans(t)
	collect := recordMetrics(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the provider was called despite an unset credential")
	}))
	defer upstream.Close()

	mapper := &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"ok":true}`)}
	step := newStep(t, &stubRegistry{plan: testPlan(upstream.URL, http.MethodGet)}, mapper,
		func(cfg *Config) {
			cfg.AuthByProvider = map[string]*AuthProfile{
				"mausamgram": {
					Scheme:        "query",
					QueryName:     "token",
					QueryValueEnv: "OBSERVE_TEST_UNSET_TOKEN",
				},
			}
		})
	if _, err := runStep(t, step, selectBody); err == nil {
		t.Fatal("Run() succeeded with no credential configured")
	}

	calls := providerCallSpans(spans())
	if len(calls) == 0 {
		t.Fatal("a credential failure produced no span at all")
	}
	if got := attrOf(t, calls[0].Attributes, attrOutcome).AsString(); got != outcomeCredential {
		t.Errorf("oan.outcome = %q, want %q -- configuration, not weather", got, outcomeCredential)
	}
	if got := sumFor(t, collect(), "oan_provider_calls_total", outcomeCredential); got == 0 {
		t.Error("calls_total{outcome=credential} = 0, want at least 1")
	}

	// The span still carries a URL, and it is the pre-credential endpoint --
	// which is safe precisely because the credential was never attached.
	url := attrOf(t, calls[0].Attributes, attrURL).AsString()
	if !strings.Contains(url, "/get-daily") {
		t.Errorf("url.full = %q, want the endpoint that was about to be called", url)
	}
	if strings.Contains(url, "token=") {
		t.Errorf("url.full = %q, but no credential was ever attached", url)
	}
}

// --- classification ---------------------------------------------------------

// These are metric label values, so the set has to stay small and the
// boundaries have to be deliberate. In particular: a 4xx is a completed call
// that failed, and a credential that could not be read never reached the wire
// at all -- calling either one "transport" would blame the network for our
// configuration.
func TestClassifyTellsTheFailuresApart(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		err    error
		want   string
	}{
		{"a 200 is success", http.StatusOK, nil, outcomeOK},
		{"a 204 is success", http.StatusNoContent, nil, outcomeOK},
		{"a 404 is the request's fault", http.StatusNotFound, nil, outcomeClientErr},
		{"a 401 is the request's fault", http.StatusUnauthorized, nil, outcomeClientErr},
		{"a 503 is the provider's fault", http.StatusServiceUnavailable, nil, outcomeServerErr},
		{"a deadline is a timeout", 0, context.DeadlineExceeded, outcomeTimeout},
		{"a cancelled caller is not a failure of ours", 0, context.Canceled, outcomeCanceled},
		{"an unset credential is configuration", 0,
			errors.New("the basic credential for mausamgram is not configured"), outcomeCredential},
		{"an unreachable host is the network", 0, errors.New("dial tcp: no route to host"), outcomeTransport},
		{"a 200 we refused is ours", http.StatusOK,
			errors.New("response exceeds the 1024 byte limit"), outcomeRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.status, tc.err); got != tc.want {
				t.Errorf("classify(%d, %v) = %q, want %q", tc.status, tc.err, got, tc.want)
			}
		})
	}
}

// A timeout wrapped by the http client still has to read as a timeout, or
// every slow provider is filed under "transport" and the distinction is lost.
func TestClassifyLooksThroughAWrappedDeadline(t *testing.T) {
	wrapped := fmt.Errorf("Get %q: %w", "http://provider.example/x", context.DeadlineExceeded)
	if got := classify(0, wrapped); got != outcomeTimeout {
		t.Errorf("classify of a wrapped deadline = %q, want %q", got, outcomeTimeout)
	}
}

// Cardinality: the status class is a label, so it has to collapse. One series
// per status code would multiply every other label by the size of the HTTP
// status registry.
func TestStatusClassCollapsesTheStatusCode(t *testing.T) {
	for status, want := range map[int]string{
		0: "none", 200: "2xx", 204: "2xx", 404: "4xx", 429: "4xx", 500: "5xx", 503: "5xx",
	} {
		if got := statusClass(status); got != want {
			t.Errorf("statusClass(%d) = %q, want %q", status, got, want)
		}
	}
}

// --- the binding on the context ---------------------------------------------

// The binding travels as a context value rather than as two more parameters on
// call() and attempt(). It has to survive that trip, or every span and every
// metric is labelled with empty strings and none of them can be grouped.
func TestTheBindingSurvivesTheTripDownToTheCall(t *testing.T) {
	provider, capability := bindingFrom(withBinding(context.Background(), testBindingKey))
	if provider != "mausamgram" {
		t.Errorf("provider = %q, want mausamgram", provider)
	}
	if capability != "openagrinet:WeatherObservation" {
		t.Errorf("capability = %q, want openagrinet:WeatherObservation", capability)
	}
}

// Unlabelled telemetry beats a panic. Nothing in this file should be able to
// fail a request that would otherwise have been served.
func TestAMissingBindingIsBlankRatherThanAFailure(t *testing.T) {
	provider, capability := bindingFrom(context.Background())
	if provider != "" || capability != "" {
		t.Errorf("got (%q, %q), want two empty strings", provider, capability)
	}
}

// --- the capability instruments ----------------------------------------------

// A precondition refusal never dials a provider, so the provider instruments
// cannot see it. Before this counter, those requests did not exist in metrics
// at all -- and a mandi select naming a district by name rather than by code is
// exactly that shape, which is a mistake callers make constantly.
func TestACapabilityRefusedBeforeTheCallIsStillCounted(t *testing.T) {
	collect := recordMetrics(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the provider must not be called when a precondition failed")
	}))
	defer upstream.Close()

	refusal := model.NewBadReqErr("", errors.New("this capability needs Agmarknet's codes"))
	mapper := &stubMapper{verifyErr: refusal, requestResult: []byte(`{}`), responseResult: []byte(`{}`)}
	step := newStep(t, &stubRegistry{plan: testPlan(upstream.URL, http.MethodGet)}, mapper)

	if _, err := runStep(t, step, selectBody); err == nil {
		t.Fatal("expected the precondition to refuse the request")
	}

	collected := collect()
	if got := sumFor(t, collected, "oan_capability_requests_total", outcomeRefused); got != 1 {
		t.Errorf("capability_requests_total{outcome=refused} = %d, want 1", got)
	}
	// The provider counter must stay empty: nothing was asked of the provider,
	// so nothing may land on its error rate.
	for _, outcome := range []string{outcomeOK, outcomeClientErr, outcomeServerErr, outcomeTransport} {
		if got := sumFor(t, collected, "oan_provider_calls_total", outcome); got != 0 {
			t.Errorf("provider_calls_total{outcome=%s} = %d, want 0 -- no provider was called", outcome, got)
		}
	}
	if got := histogramCount(t, collected, "oan_capability_duration_seconds"); got != 1 {
		t.Errorf("capability duration recorded %d observations, want 1", got)
	}
}

// A served request is counted too, so the counter is a denominator and not an
// error log: an error rate needs both halves.
func TestACapabilityThatSucceedsIsCountedAsOK(t *testing.T) {
	collect := recordMetrics(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"fcstday1":{"rain":12.4}}`)
	}))
	defer upstream.Close()

	mapper := &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"ok":true}`)}
	step := newStep(t, &stubRegistry{plan: testPlan(upstream.URL, http.MethodGet)}, mapper)
	if _, err := runStep(t, step, selectBody); err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}

	collected := collect()
	if got := sumFor(t, collected, "oan_capability_requests_total", outcomeOK); got != 1 {
		t.Errorf("capability_requests_total{outcome=ok} = %d, want 1", got)
	}
}

// The labels are named for what the CALLER got, not for the Go type that
// produced it. That is the split an operator needs: "refused" is the caller's
// payload, "upstream" is somebody else's service, and a graph that merges them
// cannot be acted on.
func TestServedOutcomeNamesWhatTheCallerGot(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"served", nil, outcomeOK},
		{"the payload was refused", model.NewBadReqErr("", errors.New("bad shape")), outcomeRefused},
		{"schema validation refused it", &model.SchemaValidationErr{
			Errors: []model.Error{{Code: "SCH_INVALID_FORMAT", Message: "no"}}}, outcomeRefused},
		{"the binding is not published", model.NewNotFoundErr("", errors.New("gone")), outcomeNotFound},
		{"the provider did not answer", model.NewCodedErr(
			http.StatusBadGateway, "NET_DOWNSTREAM_UNAVAILABLE", errors.New("no answer")), outcomeUpstream},
		{"anything else is ours", errors.New("the response half produced nothing"), outcomeInternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := servedOutcome(tc.err); got != tc.want {
				t.Errorf("servedOutcome(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// --- the transaction on the span ---------------------------------------------

// trace_id correlates one request chain. transaction_id is the Beckn thread
// across several: search, select and confirm are three traces and one
// transaction. Without it on the span, a trace backend can group the logs of a
// transaction but not its traces.
func TestTheSpanCarriesTheTransactionForGrouping(t *testing.T) {
	spans := recordSpans(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"fcstday1":{"rain":12.4}}`)
	}))
	defer upstream.Close()

	mapper := &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"ok":true}`)}
	step := newStep(t, &stubRegistry{plan: testPlan(upstream.URL, http.MethodGet)}, mapper)

	// As the request pipeline leaves it: reqpreprocessor puts the id here.
	ctx := &model.StepContext{
		Context: context.WithValue(t.Context(), model.ContextKeyTxnID, "txn-9f2c"),
		Body:    []byte(selectBody),
	}
	if err := step.Run(ctx); err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}

	calls := providerCallSpans(spans())
	if len(calls) != 1 {
		t.Fatalf("got %d provider call spans, want 1", len(calls))
	}
	if got := attrOf(t, calls[0].Attributes, attrTransaction).AsString(); got != "txn-9f2c" {
		t.Errorf("oan.transaction_id = %q, want txn-9f2c", got)
	}
}

// A deployment that does not configure reqpreprocessor has no transaction id to
// carry. Blank, never a panic: nothing in the telemetry path may fail a request
// that would otherwise have been served.
func TestAMissingTransactionIsBlankRatherThanAFailure(t *testing.T) {
	if got := transactionFrom(context.Background()); got != "" {
		t.Errorf("transactionFrom(empty) = %q, want an empty string", got)
	}
}

// --- the provider client's transport -----------------------------------------

// THE NEGATIVE CONTROL FOR THE ONE CHANGE THAT COULD ALTER BEHAVIOUR. Every
// pooling setting is optional, and a config naming none of them must leave the
// client exactly as it was before these existed -- Go's own defaults, untouched.
func TestAnUnconfiguredTransportKeepsGoesDefaults(t *testing.T) {
	t.Parallel()

	standard := http.DefaultTransport.(*http.Transport)
	got, ok := providerTransport(&Config{}).(*http.Transport)
	if !ok {
		t.Fatalf("providerTransport returned %T, want *http.Transport", got)
	}

	if got.MaxIdleConns != standard.MaxIdleConns {
		t.Errorf("MaxIdleConns = %d, want Go's default %d", got.MaxIdleConns, standard.MaxIdleConns)
	}
	if got.MaxIdleConnsPerHost != standard.MaxIdleConnsPerHost {
		t.Errorf("MaxIdleConnsPerHost = %d, want Go's default %d",
			got.MaxIdleConnsPerHost, standard.MaxIdleConnsPerHost)
	}
	if got.IdleConnTimeout != standard.IdleConnTimeout {
		t.Errorf("IdleConnTimeout = %v, want Go's default %v", got.IdleConnTimeout, standard.IdleConnTimeout)
	}
	if got.ResponseHeaderTimeout != standard.ResponseHeaderTimeout {
		t.Errorf("ResponseHeaderTimeout = %v, want Go's default %v",
			got.ResponseHeaderTimeout, standard.ResponseHeaderTimeout)
	}
}

// And each setting takes effect when an operator does name it. MaxIdleConnsPerHost
// is the one worth having: Go defaults it to 2, so past two concurrent calls to
// one provider every further call pays a fresh TCP connect and TLS handshake --
// time that lands inside the duration this step reports as the provider's.
func TestAConfiguredTransportAppliesEverySetting(t *testing.T) {
	t.Parallel()

	got, ok := providerTransport(&Config{
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 7 * time.Second,
	}).(*http.Transport)
	if !ok {
		t.Fatalf("providerTransport returned %T, want *http.Transport", got)
	}

	if got.MaxIdleConns != 256 {
		t.Errorf("MaxIdleConns = %d, want 256", got.MaxIdleConns)
	}
	if got.MaxIdleConnsPerHost != 64 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 64", got.MaxIdleConnsPerHost)
	}
	if got.IdleConnTimeout != 90*time.Second {
		t.Errorf("IdleConnTimeout = %v, want 90s", got.IdleConnTimeout)
	}
	if got.ResponseHeaderTimeout != 7*time.Second {
		t.Errorf("ResponseHeaderTimeout = %v, want 7s", got.ResponseHeaderTimeout)
	}
}

// The clone must be a clone: overriding one setting must not silently reset the
// rest of http.DefaultTransport's tuning, which is the bug a naive
// &http.Transport{...} would introduce.
func TestOverridingOneSettingLeavesTheRestOfTheDefaultAlone(t *testing.T) {
	t.Parallel()

	standard := http.DefaultTransport.(*http.Transport)
	got := providerTransport(&Config{MaxIdleConnsPerHost: 64}).(*http.Transport)

	if got.TLSHandshakeTimeout != standard.TLSHandshakeTimeout {
		t.Errorf("TLSHandshakeTimeout = %v, want Go's default %v",
			got.TLSHandshakeTimeout, standard.TLSHandshakeTimeout)
	}
	if got.ExpectContinueTimeout != standard.ExpectContinueTimeout {
		t.Errorf("ExpectContinueTimeout = %v, want Go's default %v",
			got.ExpectContinueTimeout, standard.ExpectContinueTimeout)
	}
	if got.MaxIdleConns != standard.MaxIdleConns {
		t.Errorf("MaxIdleConns = %d, want Go's default %d", got.MaxIdleConns, standard.MaxIdleConns)
	}
}
