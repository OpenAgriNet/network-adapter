// The request body and method: what to send, and in what spelling.
package util

import (
	"bytes"
	"io"
	"net/http"
	"strings"
)

// RequestBody returns the body to send, which is none for methods that take none.
func RequestBody(method string, mapped []byte) io.Reader {
	if !HasBody(method) {
		return nil
	}
	return bytes.NewReader(mapped)
}

// HasBody reports whether a method carries a request body.
func HasBody(method string) bool {
	switch CanonicalMethod(method) {
	case http.MethodGet, http.MethodHead, http.MethodDelete, "":
		return false
	default:
		return true
	}
}

// CanonicalMethod upper-cases a known HTTP method and leaves anything else
// alone.
//
// NewRequestWithContext sends the method verbatim, so a row reading "post" put
// `post /path HTTP/1.1` on the wire and most gateways answer that with 405 --
// which classifies permanent and surfaces as "provider did not answer".
//
// Unknown methods pass through: net/http already rejects an invalid token, and
// upper-casing everything would restrict an upstream entitled to a method this
// list has not heard of. An empty method stays empty, since net/http already
// treats "" as GET.
func CanonicalMethod(method string) string {
	upper := strings.ToUpper(method)
	switch upper {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodConnect,
		http.MethodOptions, http.MethodTrace:
		return upper
	}
	return method
}
