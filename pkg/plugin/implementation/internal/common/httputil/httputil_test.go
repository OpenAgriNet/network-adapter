package httputil

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

// explain still collapses whitespace, because the body it prepares now goes to
// a log line rather than an error -- an indented body would spread one failure
// over several lines either way.
func TestExplainCollapsesWhitespace(t *testing.T) {
	t.Parallel()

	got := Explain([]byte("{\n  \"message\": \"no data available\"\n}"))
	if strings.Contains(got, "\n") {
		t.Errorf("Explain(%q) left a newline in", got)
	}
	if !strings.Contains(got, "no data available") {
		t.Errorf("explain = %q, want the provider's message preserved", got)
	}
}

// A body is quoted, not dumped: a provider answering with a page of HTML must
// not put all of it in a log line or a NACK.
func TestExplainTruncatesAndHandlesAnEmptyBody(t *testing.T) {
	t.Parallel()

	if got := Explain(nil); got != "(no body)" {
		t.Errorf("Explain(nil) = %q, want a marker rather than an empty string", got)
	}
	long := Explain([]byte(strings.Repeat("x", ExplainLimit+50)))
	if len(long) > ExplainLimit+len("... (truncated)") {
		t.Errorf("explain kept %d characters, want it truncated near %d", len(long), ExplainLimit)
	}
	if !strings.HasSuffix(long, "(truncated)") {
		t.Errorf("a truncated body should say so, got %q", long[len(long)-20:])
	}
}

// The join itself: baseUrl cannot end in a slash and path must begin with one,
// so exactly one separator appears between them. Asserted so a change to either
// side cannot quietly produce a doubled or missing slash.
func TestBuildEndpointJoinsWithOneSlash(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ base, path, want string }{
		{"http://host:9100", "/get-daily", "http://host:9100/get-daily"},
		{"http://host:9100/api", "/get-daily", "http://host:9100/api/get-daily"},
		{"http://host:9100/", "/get-daily", "http://host:9100/get-daily"},
	} {
		got, err := BuildEndpoint(tc.base, model.ActionPlan{Method: http.MethodPost, Path: tc.path}, nil)
		if err != nil {
			t.Fatalf("BuildEndpoint(%q, %q) returned an unexpected error: %v", tc.base, tc.path, err)
		}
		if got != tc.want {
			t.Errorf("BuildEndpoint(%q, %q) = %q, want %q", tc.base, tc.path, got, tc.want)
		}
	}
}

// --- where the binding key lives ----------------------------------------------
//
// A default, not a setting: every participant must agree where the halves of a
// binding key sit, or two adapters disagree about what a binding key is and
// requests silently fail to match. The override exists so a spec change can be
// tracked without waiting for a release, and has to be typed deliberately.

// asQueryValue carries the three JSON scalars and refuses everything else,
// naming the field so a mapping that produced one is told which. null is in the
// refused set on purpose: a parameter present but empty and a parameter absent
// mean different things to some upstreams, and a mapping says which it wants by
// omitting the field or setting "".
func TestAsQueryRefusesWhatCannotBeAQueryParameter(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, mapped string }{
		{"an object", `{"location":{"lat":19.9975}}`},
		{"an array", `{"days":[1,2,3]}`},
		{"null", `{"station":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := asQuery([]byte(tc.mapped))
			if err == nil {
				t.Fatalf("asQuery(%s) was accepted; it cannot become a query parameter", tc.mapped)
			}
			// The field, so a mapping author knows which one to fix.
			if !strings.Contains(err.Error(), "station") &&
				!strings.Contains(err.Error(), "location") &&
				!strings.Contains(err.Error(), "days") {
				t.Errorf("error %q should name the offending field", err)
			}
		})
	}
}

// An empty string IS carried: it is a scalar, and a mapping setting one is
// asking for the parameter to be present and empty.
func TestAsQueryCarriesAnEmptyString(t *testing.T) {
	t.Parallel()

	got, err := asQuery([]byte(`{"token":""}`))
	if err != nil {
		t.Fatalf("asQuery() returned an unexpected error: %v", err)
	}
	if got != "token=" {
		t.Errorf("query = %q, want token= -- an empty string is a value, not an absence", got)
	}
}

func TestAsQueryRendersScalarsWithoutInventingPrecision(t *testing.T) {
	t.Parallel()

	got, err := asQuery([]byte(`{"lat":19.9975,"count":3,"name":"imd","live":true}`))
	if err != nil {
		t.Fatalf("asQuery() returned an unexpected error: %v", err)
	}
	for _, want := range []string{"lat=19.9975", "count=3", "name=imd", "live=true"} {
		if !strings.Contains(got, want) {
			t.Errorf("query %q is missing %q", got, want)
		}
	}
}

func TestAsQueryHandlesAnEmptyMapping(t *testing.T) {
	t.Parallel()

	got, err := asQuery([]byte(`{}`))
	if err != nil || got != "" {
		t.Errorf("asQuery({}) = (%q, %v), want an empty query and no error", got, err)
	}
}

// Both bounds come from a registry row, so both are data. An attempt holds a
// goroutine and the inbound connection for its whole timeout, and the server's
// write timeout does not cancel the request context -- so retryMax 1000 with
// timeoutMs 60000 is one row deciding this process is busy for seventeen hours.
func TestBudgetClampsWhatTheRegistryAsksFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		timeoutMs   int
		retryMax    int
		wantTimeout time.Duration
		wantRetries int
	}{
		{"absent uses the contract's defaults", 0, 0, DefaultTimeout, DefaultRetryMax},
		{"within the ceilings is honoured", 2000, 3, 2 * time.Second, 3},
		{"exactly at the ceilings is honoured", int(MaxTimeout / time.Millisecond), MaxRetryMax, MaxTimeout, MaxRetryMax},
		{"a timeout past the ceiling is clamped", 600000, 0, MaxTimeout, DefaultRetryMax},
		{"retries past the ceiling are clamped", 0, 1000, DefaultTimeout, MaxRetryMax},
		{"both past the ceiling are clamped", 600000, 1000, MaxTimeout, MaxRetryMax},
		{"negative values fall back to the defaults", -1, -1, DefaultTimeout, DefaultRetryMax},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotTimeout, gotRetries := Budget(model.ActionPlan{
				TimeoutMs: tt.timeoutMs, RetryMax: tt.retryMax,
			})
			if gotTimeout != tt.wantTimeout {
				t.Errorf("timeout = %v, want %v", gotTimeout, tt.wantTimeout)
			}
			if gotRetries != tt.wantRetries {
				t.Errorf("retries = %d, want %d", gotRetries, tt.wantRetries)
			}
		})
	}
}

// The shift this replaced overflowed int64 once it reached 38 at a 50ms base.
// The wrapped value is NEGATIVE, so it passed the ceiling check and was
// returned, and a sleep on a negative duration returns immediately -- the
// retry loop then spun as fast as the provider could refuse. Past 64 it
// yielded 0, with the same effect. Worse, it was not monotonic: attempt 40
// wrapped back to a sane 800ms, so the symptom came and went by attempt count.
//
// The clamp on retryMax now keeps attempts to MaxRetryMax+1, which puts the
// overflow out of reach through call(). This is asserted anyway, because
// backoff is a package function and a ceiling somewhere else is not a
// property of this one.
func TestBackoffNeverReturnsANonPositiveDuration(t *testing.T) {
	t.Parallel()

	for _, attempt := range []int{0, 1, 2, 3, 4, 5, 6, 37, 38, 39, 40, 63, 64, 65, 100, 1000} {
		got := Backoff(attempt)
		if got <= 0 {
			t.Errorf("Backoff(%d) = %v; a non-positive wait makes the retry loop spin", attempt, got)
		}
		if got > RetryBackoffMax {
			t.Errorf("Backoff(%d) = %v, above the %v ceiling", attempt, got, RetryBackoffMax)
		}
	}
}

// The doubling itself, which the overflow fix must not have changed.
func TestBackoffDoublesToTheCeiling(t *testing.T) {
	t.Parallel()

	want := []time.Duration{
		50 * time.Millisecond,  // attempt 1
		100 * time.Millisecond, // 2
		200 * time.Millisecond, // 3
		400 * time.Millisecond, // 4
		800 * time.Millisecond, // 5, at the ceiling
		800 * time.Millisecond, // 6, held there
	}
	for i, w := range want {
		if got := Backoff(i + 1); got != w {
			t.Errorf("Backoff(%d) = %v, want %v", i+1, got, w)
		}
	}
}

// hasBody upper-cased privately, which made the method look case-insensitive
// when it is not: NewRequestWithContext transmits it verbatim, so a registry
// row reading `method: "post"` sent `post /path HTTP/1.1`. The body was
// attached correctly, and nginx answered 405 -- classified permanent, and
// surfacing as a 502 "provider did not answer".
func TestCanonicalMethodFixesTheCaseTheRowWasWrittenIn(t *testing.T) {
	t.Parallel()

	tests := []struct{ in, want string }{
		{"post", http.MethodPost},
		{"PoSt", http.MethodPost},
		{"POST", http.MethodPost},
		{"get", http.MethodGet},
		{"delete", http.MethodDelete},
		{"patch", http.MethodPatch},
		// Left alone: upper-casing everything would restrict an upstream
		// entitled to a method this list has not heard of.
		{"FrobNicate", "FrobNicate"},
		// Empty stays empty; net/http documents "" as GET and substitutes it.
		{"", ""},
	}
	for _, tt := range tests {
		if got := CanonicalMethod(tt.in); got != tt.want {
			t.Errorf("CanonicalMethod(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// The registry publishes two urls per action and only one of them was checked.
// jsonmapper has always validated its mapping reference this way; this is the
// same check on the base url beside it.
func TestVerifyBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		baseURL string
		wantErr bool
	}{
		{"http is fine", "http://provider:9100", false},
		{"https is fine", "https://provider.example.com/api", false},
		{"empty is refused", "", true},
		// The case from the review: a scheme left off. Without this check it
		// failed inside NewRequestWithContext and arrived as a 502.
		{"a host and port with no scheme is refused", "registry:8081", true},
		{"a bare host is refused", "provider", true},
		{"a scheme that is not http is refused", "file:///etc/passwd", true},
		{"a scheme with no host is refused", "http://", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := verifyBaseURL(tt.baseURL)
			if tt.wantErr && err == nil {
				t.Errorf("verifyBaseURL(%q) = nil, want an error", tt.baseURL)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("verifyBaseURL(%q) = %v, want nil", tt.baseURL, err)
			}
		})
	}
}

// A dot segment would be resolved by net/url, so the request that left would
// not be the request the row described. A fragment is never sent at all.
func TestVerifyPathRefusesDotSegmentsAndFragments(t *testing.T) {
	t.Parallel()

	for _, path := range []string{
		"/../admin",
		"/v1/../../etc",
		"/v1/./get-daily",
		"/get-daily#section",
	} {
		if err := verifyPath(path); err == nil {
			t.Errorf("verifyPath(%q) = nil, want it refused", path)
		}
	}
	// A dot inside a segment is an ordinary character and must still pass.
	for _, path := range []string{"/v1/get-daily", "/v1/data.json", "/a..b"} {
		if err := verifyPath(path); err != nil {
			t.Errorf("verifyPath(%q) = %v, want nil", path, err)
		}
	}
}
