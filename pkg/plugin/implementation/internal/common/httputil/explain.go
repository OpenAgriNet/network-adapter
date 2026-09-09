// Quoting a failed response body for a human.
package httputil

import "strings"

// Explain renders a failed response body for a human.
//
// The status says a call failed and nothing about why. The provider's own
// account is in the body -- Agmarknet says "no data" in the body of a 400.
func Explain(body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return "(no body)"
	}
	// Collapse whitespace: indented JSON or an HTML error page should not
	// spread one failure over forty log lines.
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > ExplainLimit {
		return text[:ExplainLimit] + "... (truncated)"
	}
	return text
}
