// The request body and method: what to send, and in what spelling.
package util

import (
	"bytes"
	"io"
	"net/http"
	"strings"
)

// RequestBody returns the body to send: none for a method that takes none,
// and otherwise the mapped request less the fields the mapping named for the
// query string. None again when that leaves nothing, so a call whose whole
// request is a query parameter does not also announce an empty JSON body.
//
// A request that cannot be split is reported by BuildEndpoint, which asks the
// same question first and fails the call before this is reached.
func RequestBody(method string, mapped []byte) io.Reader {
	if !HasBody(method) {
		return nil
	}
	_, body, err := Split(mapped, true)
	if err != nil || len(body) == 0 {
		return nil
	}
	return bytes.NewReader(body)
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
