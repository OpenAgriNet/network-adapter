package pipeline

// classify_test.go pins the error classification a pipeline file DECLARES.
//
// Until this existed, "no rows for this region" was recognised by matching one
// upstream's literal string in Go. That made the engine unusable by a second
// provider whose upstream says it differently -- and it is the failure that
// once turned 27 of 36 quiet regions into 27 reported outages, so getting it
// from the file rather than from a constant is the whole point.

import (
	"errors"
	"net/http"
	"testing"
)

// The rules mandi declares, as a fixture: a specific 400 means empty, a dead
// token means reauth, everything else is an outage.
func agmarknetRules() []ErrorRule {
	return []ErrorRule{
		{When: &ErrorMatch{Status: 400, BodyContains: "No data available."}, Classify: classifyEmpty},
		{When: &ErrorMatch{Status: []any{401, 403}}, Classify: classifyReauth},
		{Default: classifyTransport},
	}
}

func TestClassifyFollowsTheDeclaredRules(t *testing.T) {
	tests := map[string]struct {
		status int
		body   string
		want   string
	}{
		"the declared empty answer":       {400, `{"success":false,"message":"No data available."}`, classifyEmpty},
		"a 400 that is NOT the empty one": {400, `{"error":"bad state code"}`, classifyTransport},
		"a dead token":                    {401, `{"error":"unauthorized"}`, classifyReauth},
		"the other dead-token status":     {403, `{"error":"forbidden"}`, classifyReauth},
		"an outage":                       {500, `{"error":"upstream is down"}`, classifyTransport},
		"a success is classified as none": {200, `[]`, ""},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := classify(agmarknetRules(), tc.status, []byte(tc.body))
			if got != tc.want {
				t.Errorf("classify(%d, %q) = %q, want %q", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// A provider whose upstream says "empty" completely differently must need no
// engine change. This is the test that proves the hardcoding is gone.
func TestClassifyServesADifferentUpstream(t *testing.T) {
	// A 204 with no body, and a 404 meaning "nothing here" rather than an
	// outage -- both plausible, neither anything like Agmarknet's shape.
	rules := []ErrorRule{
		{When: &ErrorMatch{Status: 204}, Classify: classifyEmpty},
		{When: &ErrorMatch{Status: 404}, Classify: classifyEmpty},
		{Default: classifyTransport},
	}

	if got := classify(rules, 204, nil); got != classifyEmpty {
		t.Errorf("204 = %q, want %q", got, classifyEmpty)
	}
	if got := classify(rules, 404, []byte(`not found`)); got != classifyEmpty {
		t.Errorf("404 = %q, want %q", got, classifyEmpty)
	}
	if got := classify(rules, 400, []byte(`{"message":"No data available."}`)); got != classifyTransport {
		t.Errorf("Agmarknet's own empty-marker leaked into another provider's rules: got %q", got)
	}
}

// bodyContains without a status matches on the body alone; status without a
// body matches on status alone. Both are useful and both must work.
func TestClassifyMatchesOnEitherHalf(t *testing.T) {
	byBody := []ErrorRule{
		{When: &ErrorMatch{BodyContains: "quota exceeded"}, Classify: classifyTransport},
		{Default: classifyEmpty},
	}
	if got := classify(byBody, 200, []byte(`{"error":"quota exceeded today"}`)); got != classifyTransport {
		t.Errorf("a body-only rule did not match: %q", got)
	}

	byStatus := []ErrorRule{
		{When: &ErrorMatch{Status: 429}, Classify: classifyTransport},
		{Default: classifyEmpty},
	}
	if got := classify(byStatus, 429, nil); got != classifyTransport {
		t.Errorf("a status-only rule did not match: %q", got)
	}
}

// Rules are ordered and the FIRST match wins, so a file reads top to bottom.
func TestClassifyTakesTheFirstMatch(t *testing.T) {
	rules := []ErrorRule{
		{When: &ErrorMatch{Status: 400, BodyContains: "specific"}, Classify: classifyEmpty},
		{When: &ErrorMatch{Status: 400}, Classify: classifyTransport},
		{Default: classifyTransport},
	}
	if got := classify(rules, 400, []byte("a specific thing")); got != classifyEmpty {
		t.Errorf("the second rule won over the first: %q", got)
	}
}

// A file declaring NO rules must not silently treat every failure as empty --
// that would swallow outages. With nothing declared, a non-2xx is an outage.
func TestClassifyWithNoRulesTreatsFailureAsAnOutage(t *testing.T) {
	if got := classify(nil, 500, []byte("boom")); got != classifyTransport {
		t.Errorf("with no declared rules a 500 classified as %q, want %q", got, classifyTransport)
	}
	if got := classify(nil, 200, []byte("[]")); got != "" {
		t.Errorf("with no declared rules a 200 classified as %q, want none", got)
	}
}

// The sentinel a step's onError dispatches on must still be produced, and must
// still survive wrapping -- errors.Is is how a quiet region is told from a
// broken one, several call frames away.
func TestClassifiedEmptyStillMatchesTheSentinel(t *testing.T) {
	err := classifiedError(classifyEmpty, http.StatusBadRequest, "GET /things")
	if !errors.Is(err, ErrNoUpstreamData) {
		t.Fatal("an emptyResult no longer satisfies errors.Is(ErrNoUpstreamData); " +
			"every quiet region would now read as an outage")
	}

	outage := classifiedError(classifyTransport, http.StatusInternalServerError, "GET /things")
	if errors.Is(outage, ErrNoUpstreamData) {
		t.Fatal("an outage satisfies the empty sentinel; a real failure would be counted as quiet")
	}
}

// Whatever the upstream put in its body, it must never reach an error: this
// class of upstream echoes the request back, and the request carries the token.
func TestClassifiedErrorNeverQuotesTheBody(t *testing.T) {
	const secret = "tok-SECRET-do-not-leak-91af"
	err := classifiedError(classifyTransport, 400, "GET /things?token="+secret)
	if err == nil {
		t.Fatal("want an error")
	}
	if contains(err.Error(), secret) {
		t.Errorf("the token leaked into an error: %v", err)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}()
}

// A rule's status is written as one code or a list, and YAML or JSON may hand
// either back as int, int64 or float64. Every spelling must match the same
// way, or `status: [401, 403]` from one parser would silently match nothing.
func TestStatusMatchesEverySpellingOfAStatus(t *testing.T) {
	for name, tc := range map[string]struct {
		stated any
		want   bool
	}{
		"int":              {401, true},
		"int, other":       {403, false},
		"int64":            {int64(401), true},
		"float64 (JSON)":   {float64(401), true},
		"list of any":      {[]any{403, float64(401)}, true},
		"list of any, no":  {[]any{403, 500}, false},
		"list of int":      {[]int{400, 401}, true},
		"list of int, no":  {[]int{400}, false},
		"list with a word": {[]any{"401"}, false},
		"a word":           {"401", false},
		"nothing":          {nil, false},
	} {
		if got := statusMatches(tc.stated, 401); got != tc.want {
			t.Errorf("%s: statusMatches(%v, 401) = %v, want %v", name, tc.stated, got, tc.want)
		}
	}
}
