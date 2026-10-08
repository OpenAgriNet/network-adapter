package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
)

// InprocScheme is the URL scheme for a hop to another module of this same
// process. A routing target of inproc://provider sends the request to the
// module mounted at /provider/, as a function call on the process's own
// ServeMux -- no socket, no port, no TCP.
//
// The router appends the action as for any url target, so
// inproc://provider + "select" arrives here as host "provider", path "/select",
// and is served as /provider/select. The module strips its own base path from
// there, exactly as it does for a request from the network.
//
// Only routing config may name an inproc:// target. The router refuses it in a
// request-supplied URI (bppUri/bapUri), so a caller cannot use it to reach a
// module directly.
const InprocScheme = "inproc"

// maxInprocDepth is the most in-process hops one request chain may take. The
// longest chain today is one hop (provider publish -> network publish).
const maxInprocDepth = 5

// inprocDepthKey carries the hop count in the request context. It lives in the
// context rather than a header so a caller cannot set or reset it.
type inprocDepthKey struct{}

func inprocDepth(ctx context.Context) int {
	d, _ := ctx.Value(inprocDepthKey{}).(int)
	return d
}

// inprocHandler is the target of inproc:// routes: the ServeMux every module is
// mounted on. It is set once after registration (SetInprocHandler), because
// each module's HTTP client is built while the mux is still being filled.
var inprocHandler atomic.Pointer[http.Handler]

// SetInprocHandler makes h the target of inproc:// routes. Call it once, after
// every module is registered on h.
func SetInprocHandler(h http.Handler) { inprocHandler.Store(&h) }

// inprocTransport serves inproc:// requests with the in-process handler and
// hands every other request to next unchanged.
type inprocTransport struct {
	next http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
//
// The inner call does not pass through http.Server, so it reproduces what a
// network hop would give the target module:
//   - a context with the caller's deadline and cancellation but none of its
//     values, as a fresh server request would have (trace context still
//     travels in the headers the transport wrapper injected);
//   - a recovered panic, turned into an error so the proxy answers 502, as for
//     an unreachable upstream.
func (t *inprocTransport) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	if req.URL.Scheme != InprocScheme {
		return t.next.RoundTrip(req)
	}
	hp := inprocHandler.Load()
	if hp == nil {
		return nil, fmt.Errorf("inproc: no in-process handler registered for %s", req.URL)
	}

	depth := inprocDepth(req.Context())
	if depth >= maxInprocDepth {
		return nil, fmt.Errorf("inproc: more than %d in-process hops for %s, is there a routing loop?", maxInprocDepth, req.URL)
	}

	ctx, cancel := detachedContext(req.Context())
	defer cancel()
	ctx = context.WithValue(ctx, inprocDepthKey{}, depth+1)

	// inproc://provider/select -> /provider/select, the path the target
	// module is mounted under. Clone so the caller's request is left as is.
	in := req.Clone(ctx)
	in.URL.Scheme = ""
	in.URL.Host = ""
	in.URL.Path = "/" + req.URL.Host + req.URL.Path
	in.URL.RawPath = ""
	in.RequestURI = in.URL.RequestURI()
	in.Host = req.URL.Host
	in.RemoteAddr = "inproc"

	defer func() {
		if p := recover(); p != nil {
			resp, err = nil, fmt.Errorf("inproc: handler for %s panicked: %v", req.URL, p)
		}
	}()

	// The response is buffered. Beckn payloads are small, and the proxy reads
	// the whole upstream body in its ModifyResponse step anyway.
	rec := httptest.NewRecorder()
	(*hp).ServeHTTP(rec, in)

	resp = rec.Result()
	resp.Request = req
	return resp, nil
}

// detachedContext returns a context that is cancelled when parent is (and
// shares its deadline) but carries none of parent's values.
func detachedContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	if dl, ok := parent.Deadline(); ok {
		var cancelDL context.CancelFunc
		ctx, cancelDL = context.WithDeadline(ctx, dl)
		prev := cancel
		cancel = func() { cancelDL(); prev() }
	}
	stop := context.AfterFunc(parent, cancel)
	return ctx, func() { stop(); cancel() }
}
