package handler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// fakeRoundTripper records whether it was called, standing in for the network.
type fakeRoundTripper struct{ called bool }

func (f *fakeRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	f.called = true
	return &http.Response{StatusCode: http.StatusTeapot, Body: io.NopCloser(strings.NewReader(""))}, nil
}

// networkTransport returns the *http.Transport under the inproc layer of a
// client built by newHTTPClient with no wrapper -- the transport the network
// settings (idle conns, timeouts) are applied to.
func networkTransport(t *testing.T, client *http.Client) *http.Transport {
	t.Helper()
	ip, ok := client.Transport.(*inprocTransport)
	if !ok {
		t.Fatalf("client transport is %T, want *inprocTransport", client.Transport)
	}
	tr, ok := ip.next.(*http.Transport)
	if !ok {
		t.Fatalf("network transport is %T, want *http.Transport", ip.next)
	}
	return tr
}

// withInprocHandler installs h for one test and restores the previous handler.
func withInprocHandler(t *testing.T, h http.Handler) {
	t.Helper()
	prev := inprocHandler.Load()
	if h == nil {
		inprocHandler.Store(nil)
	} else {
		SetInprocHandler(h)
	}
	t.Cleanup(func() { inprocHandler.Store(prev) })
}

func newInprocRequest(t *testing.T, ctx context.Context, rawURL string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(`{"a":1}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return req
}

func TestInproc_NonInprocPassesThrough(t *testing.T) {
	served := false
	withInprocHandler(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served = true }))
	next := &fakeRoundTripper{}

	resp, err := (&inprocTransport{next: next}).RoundTrip(newInprocRequest(t, context.Background(), "http://example.org/select"))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if !next.called || served || resp.StatusCode != http.StatusTeapot {
		t.Fatalf("http:// must go to next only: next=%v served=%v status=%d", next.called, served, resp.StatusCode)
	}
}

func TestInproc_PathRewriteAndBody(t *testing.T) {
	var gotURI, gotBody string
	mux := http.NewServeMux()
	mux.HandleFunc("/provider/", func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.URL.RequestURI()
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	})
	withInprocHandler(t, mux)
	next := &fakeRoundTripper{}

	resp, err := (&inprocTransport{next: next}).RoundTrip(newInprocRequest(t, context.Background(), "inproc://provider/select?x=1"))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if gotURI != "/provider/select?x=1" {
		t.Errorf("handler saw %q, want /provider/select?x=1", gotURI)
	}
	if gotBody != `{"a":1}` {
		t.Errorf("handler body = %q", gotBody)
	}
	if next.called {
		t.Error("inproc:// must not reach the network transport")
	}
}

func TestInproc_ResponsePassedBack(t *testing.T) {
	withInprocHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test", "yes")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	req := newInprocRequest(t, context.Background(), "inproc://network/publish")

	resp, err := (&inprocTransport{next: &fakeRoundTripper{}}).RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusAccepted || resp.Header.Get("X-Test") != "yes" || string(body) != `{"ok":true}` {
		t.Fatalf("got status=%d header=%q body=%q", resp.StatusCode, resp.Header.Get("X-Test"), body)
	}
	if resp.Request != req {
		t.Error("resp.Request must be the original request")
	}
}

func TestInproc_ContextReachesHandler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var gotErr error
	withInprocHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotErr = r.Context().Err()
	}))

	if _, err := (&inprocTransport{next: &fakeRoundTripper{}}).RoundTrip(newInprocRequest(t, ctx, "inproc://provider/select")); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if !errors.Is(gotErr, context.Canceled) {
		t.Fatalf("handler ctx err = %v, want context.Canceled", gotErr)
	}
}

func TestInproc_DepthIncrementsInContext(t *testing.T) {
	var got int
	withInprocHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = inprocDepth(r.Context())
	}))
	ctx := context.WithValue(context.Background(), inprocDepthKey{}, 2)

	if _, err := (&inprocTransport{next: &fakeRoundTripper{}}).RoundTrip(newInprocRequest(t, ctx, "inproc://provider/select")); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if got != 3 {
		t.Fatalf("inner depth = %d, want 3", got)
	}
}

func TestInproc_DepthGuard(t *testing.T) {
	served := false
	withInprocHandler(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served = true }))
	ctx := context.WithValue(context.Background(), inprocDepthKey{}, maxInprocDepth)

	if _, err := (&inprocTransport{next: &fakeRoundTripper{}}).RoundTrip(newInprocRequest(t, ctx, "inproc://provider/select")); err == nil {
		t.Fatal("want error at max depth")
	}
	if served {
		t.Error("handler must not run past max depth")
	}
}

// A caller must not be able to influence the hop count: a header with a
// negative or huge value is ignored.
func TestInproc_DepthHeaderIgnored(t *testing.T) {
	var got int
	withInprocHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = inprocDepth(r.Context())
	}))
	req := newInprocRequest(t, context.Background(), "inproc://provider/select")
	req.Header.Set("X-Inproc-Depth", "-1000")

	if _, err := (&inprocTransport{next: &fakeRoundTripper{}}).RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if got != 1 {
		t.Fatalf("inner depth = %d, want 1", got)
	}
}

// A self-routing module stops at the guard instead of recursing.
func TestInproc_LoopStops(t *testing.T) {
	calls := 0
	tr := &inprocTransport{next: &fakeRoundTripper{}}
	var h http.HandlerFunc
	h = func(w http.ResponseWriter, r *http.Request) {
		calls++
		if _, err := tr.RoundTrip(newInprocRequest(t, r.Context(), "inproc://loop/x")); err != nil {
			w.WriteHeader(http.StatusBadGateway)
		}
	}
	withInprocHandler(t, h)

	if _, err := tr.RoundTrip(newInprocRequest(t, context.Background(), "inproc://loop/x")); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if calls != maxInprocDepth {
		t.Fatalf("handler ran %d times, want %d", calls, maxInprocDepth)
	}
}

type leakKey struct{}

// The target module gets the caller's deadline but none of its context values,
// as a fresh request from the network would.
func TestInproc_ContextValuesNotLeaked(t *testing.T) {
	var leaked any
	var hasDeadline bool
	withInprocHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Context().Value(leakKey{})
		_, hasDeadline = r.Context().Deadline()
	}))
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), leakKey{}, "secret"), time.Minute)
	defer cancel()

	if _, err := (&inprocTransport{next: &fakeRoundTripper{}}).RoundTrip(newInprocRequest(t, ctx, "inproc://provider/select")); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if leaked != nil {
		t.Errorf("caller context value leaked: %v", leaked)
	}
	if !hasDeadline {
		t.Error("caller deadline must reach the target module")
	}
}

func TestInproc_NoHandler(t *testing.T) {
	withInprocHandler(t, nil)
	if _, err := (&inprocTransport{next: &fakeRoundTripper{}}).RoundTrip(newInprocRequest(t, context.Background(), "inproc://provider/select")); err == nil {
		t.Fatal("want error when no in-process handler is registered")
	}
}

func TestInproc_UnknownModuleIs404(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/provider/", func(http.ResponseWriter, *http.Request) {})
	withInprocHandler(t, mux)

	resp, err := (&inprocTransport{next: &fakeRoundTripper{}}).RoundTrip(newInprocRequest(t, context.Background(), "inproc://nope/x"))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestInproc_HandlerPanicBecomesError(t *testing.T) {
	withInprocHandler(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	if _, err := (&inprocTransport{next: &fakeRoundTripper{}}).RoundTrip(newInprocRequest(t, context.Background(), "inproc://provider/select")); err == nil {
		t.Fatal("want error from a panicking handler")
	}
}

func TestNewHTTPClient_ServesInproc(t *testing.T) {
	withInprocHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	client := newHTTPClient(&HttpClientConfig{}, nil)

	resp, err := client.Transport.RoundTrip(newInprocRequest(t, context.Background(), "inproc://provider/select"))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// recordingWrapper is a TransportWrapper that counts calls, so the test can
// check the wrapper still sees inproc:// hops (a tracing wrapper must).
type recordingWrapper struct{ calls int }

func (w *recordingWrapper) Wrap(base http.RoundTripper) http.RoundTripper {
	return roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		w.calls++
		return base.RoundTrip(r)
	})
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNewHTTPClient_WrapperSeesInproc(t *testing.T) {
	withInprocHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	wrapper := &recordingWrapper{}
	client := newHTTPClient(&HttpClientConfig{}, wrapper)

	if _, err := client.Transport.RoundTrip(newInprocRequest(t, context.Background(), "inproc://provider/select")); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if wrapper.calls != 1 {
		t.Fatalf("wrapper calls = %d, want 1", wrapper.calls)
	}
}

// TestProxy_InprocRoute drives the real proxy() (httputil.ReverseProxy) with an
// inproc:// route, the path a module takes when its router resolves to another
// module of the same process.
func TestProxy_InprocRoute(t *testing.T) {
	var gotURI, gotBody string
	mux := http.NewServeMux()
	mux.HandleFunc("/provider/", func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.URL.RequestURI()
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"answer":42}`)
	})
	withInprocHandler(t, mux)

	target, err := url.Parse("inproc://provider/select")
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	body := []byte(`{"context":{"action":"select"}}`)
	in := httptest.NewRequest(http.MethodPost, "/consumer/select", bytes.NewReader(body))
	ctx := &model.StepContext{Context: in.Context(), Request: in, Body: body, Route: &model.Route{TargetType: "url", URL: target}}
	rec := httptest.NewRecorder()
	var respBody []byte

	proxy(ctx, in, rec, newHTTPClient(&HttpClientConfig{}, nil), nil, &respBody)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	if gotURI != "/provider/select" || gotBody != string(body) {
		t.Fatalf("provider saw uri=%q body=%q", gotURI, gotBody)
	}
	if rec.Body.String() != `{"answer":42}` || string(respBody) != `{"answer":42}` {
		t.Fatalf("caller got %q, captured %q", rec.Body.String(), respBody)
	}
}

func TestInproc_TimeoutBoundsHungModule(t *testing.T) {
	withInprocHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // a module that only returns when cancelled
		w.WriteHeader(http.StatusGatewayTimeout)
	}))
	tr := &inprocTransport{next: &fakeRoundTripper{}, timeout: 50 * time.Millisecond}

	start := time.Now()
	if _, err := tr.RoundTrip(newInprocRequest(t, context.Background(), "inproc://provider/select")); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("hung module blocked the caller for %v", d)
	}
}

func TestInproc_DefaultTimeoutApplied(t *testing.T) {
	var deadline time.Time
	var ok bool
	withInprocHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadline, ok = r.Context().Deadline()
	}))
	if _, err := (&inprocTransport{next: &fakeRoundTripper{}}).RoundTrip(newInprocRequest(t, context.Background(), "inproc://provider/select")); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if !ok || time.Until(deadline) > defaultInprocTimeout {
		t.Fatalf("deadline set=%v in=%v, want within %v", ok, time.Until(deadline), defaultInprocTimeout)
	}
}

func TestNewHTTPClient_InprocTimeoutFromConfig(t *testing.T) {
	client := newHTTPClient(&HttpClientConfig{ResponseHeaderTimeout: 7 * time.Second}, nil)
	ip, ok := client.Transport.(*inprocTransport)
	if !ok || ip.timeout != 7*time.Second {
		t.Fatalf("inproc timeout = %v, want 7s", ip)
	}
}

func TestInproc_TraceContextInjected(t *testing.T) {
	prev := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTextMapPropagator(prev) })

	var got string
	withInprocHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("traceparent")
	}))
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1},
		SpanID:     trace.SpanID{2},
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	if _, err := (&inprocTransport{next: &fakeRoundTripper{}}).RoundTrip(newInprocRequest(t, ctx, "inproc://provider/select")); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if !strings.Contains(got, sc.TraceID().String()) {
		t.Fatalf("traceparent = %q, want trace id %s", got, sc.TraceID())
	}
}

func TestInproc_AbortHandlerRepanics(t *testing.T) {
	withInprocHandler(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	defer func() {
		if p := recover(); p != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want http.ErrAbortHandler re-raised", p)
		}
	}()
	_, _ = (&inprocTransport{next: &fakeRoundTripper{}}).RoundTrip(newInprocRequest(t, context.Background(), "inproc://provider/select"))
	t.Fatal("RoundTrip returned; want panic")
}

type closeTracker struct {
	io.Reader
	closed bool
}

func (c *closeTracker) Close() error { c.closed = true; return nil }

func TestInproc_RequestBodyClosed(t *testing.T) {
	for name, handler := range map[string]http.Handler{
		"served":     http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		"no handler": nil,
	} {
		t.Run(name, func(t *testing.T) {
			withInprocHandler(t, handler)
			body := &closeTracker{Reader: strings.NewReader("{}")}
			req := newInprocRequest(t, context.Background(), "inproc://provider/select")
			req.Body = body
			_, _ = (&inprocTransport{next: &fakeRoundTripper{}}).RoundTrip(req)
			if !body.closed {
				t.Fatal("request body not closed")
			}
		})
	}
}

// excludeAction with a bare inproc://provider target has no path; it must reach
// the module, not ServeMux's redirect from /provider to /provider/.
func TestInproc_BareTargetReachesModuleRoot(t *testing.T) {
	var gotURI string
	mux := http.NewServeMux()
	mux.HandleFunc("/provider/", func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.URL.RequestURI()
		w.WriteHeader(http.StatusOK)
	})
	withInprocHandler(t, mux)

	resp, err := (&inprocTransport{next: &fakeRoundTripper{}}).RoundTrip(newInprocRequest(t, context.Background(), "inproc://provider"))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != http.StatusOK || gotURI != "/provider/" {
		t.Fatalf("status=%d uri=%q, want 200 /provider/", resp.StatusCode, gotURI)
	}
}

func TestInproc_RemoteAddrIsHostPort(t *testing.T) {
	var got string
	withInprocHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.RemoteAddr }))
	if _, err := (&inprocTransport{next: &fakeRoundTripper{}}).RoundTrip(newInprocRequest(t, context.Background(), "inproc://provider/select")); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if _, _, err := net.SplitHostPort(got); err != nil {
		t.Fatalf("RemoteAddr %q is not host:port: %v", got, err)
	}
}
