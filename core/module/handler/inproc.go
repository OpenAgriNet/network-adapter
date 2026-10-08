package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
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
const InprocScheme = "inproc"

// inprocDepthHeader counts in-process hops on one request chain, so a routing
// loop (a module routing to itself, or two modules routing to each other)
// fails fast instead of recursing until the stack runs out.
const inprocDepthHeader = "X-Inproc-Depth"

// maxInprocDepth is the most in-process hops one request may take. The longest
// chain today is one hop (provider publish -> network publish).
const maxInprocDepth = 5

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
// The inner call does not pass through http.Server, so two things the server
// would normally give are done here: a panic in the target module is recovered
// into an error (the proxy answers 502, as for an unreachable upstream), and
// the request's context is the only deadline -- the same deadline a proxied
// network call is bound by.
func (t *inprocTransport) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	if req.URL.Scheme != InprocScheme {
		return t.next.RoundTrip(req)
	}
	hp := inprocHandler.Load()
	if hp == nil {
		return nil, fmt.Errorf("inproc: no in-process handler registered for %s", req.URL)
	}

	depth, _ := strconv.Atoi(req.Header.Get(inprocDepthHeader))
	if depth >= maxInprocDepth {
		return nil, fmt.Errorf("inproc: more than %d in-process hops for %s, is there a routing loop?", maxInprocDepth, req.URL)
	}

	// inproc://provider/select -> /provider/select, the path the target
	// module is mounted under. Clone so the caller's request is left as is.
	in := req.Clone(req.Context())
	in.URL.Scheme = ""
	in.URL.Host = ""
	in.URL.Path = "/" + req.URL.Host + req.URL.Path
	in.URL.RawPath = ""
	in.RequestURI = in.URL.RequestURI()
	in.Host = req.URL.Host
	in.RemoteAddr = "inproc"
	in.Header.Set(inprocDepthHeader, strconv.Itoa(depth+1))

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
