package providerstatus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

type fakeLister struct {
	mu    sync.Mutex
	ids   []string
	err   error
	calls int

	// The restriction of the last call.
	participantType, role string
}

func (f *fakeLister) ActiveProviderIDs(ctx context.Context, participantType, role string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.participantType, f.role = participantType, role
	return f.ids, f.err
}

// bareRegistry implements RegistryLookup only, so it cannot list providers.
type bareRegistry struct{}

func (bareRegistry) Lookup(ctx context.Context, req *model.Subscription) ([]model.Subscription, error) {
	return nil, nil
}

// listingRegistry is a RegistryLookup that can also list active providers.
type listingRegistry struct {
	bareRegistry
	*fakeLister
}

func newStep(t *testing.T, l *fakeLister) definition.Step {
	t.Helper()
	return newStepWith(t, l, nil)
}

// newStepWith builds the step from plugin config, as cmd/plugin.go does.
func newStepWith(t *testing.T, l *fakeLister, config map[string]string) definition.Step {
	t.Helper()
	cfg, err := BuildConfig(config)
	if err != nil {
		t.Fatalf("BuildConfig: %v", err)
	}
	s, closeFn, err := New(listingRegistry{fakeLister: l}, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = closeFn() })
	// New loads the list once; count only what requests ask from here on.
	l.mu.Lock()
	l.calls = 0
	l.mu.Unlock()
	return s
}

func publishBody(action string, providerIDs ...string) []byte {
	catalogs := make([]string, 0, len(providerIDs))
	for _, id := range providerIDs {
		if id == "" {
			catalogs = append(catalogs, `{"id":"c"}`)
			continue
		}
		catalogs = append(catalogs, fmt.Sprintf(`{"id":"c","provider":{"id":%q}}`, id))
	}
	return []byte(fmt.Sprintf(`{"context":{"action":%q},"message":{"catalogs":[%s]}}`,
		action, strings.Join(catalogs, ",")))
}

// stepCtx is a StepContext for a request to the given path, as the handler
// builds it: the full URL path, module base path included.
func stepCtx(path string, body []byte) *model.StepContext {
	return &model.StepContext{
		Context: context.Background(),
		Request: httptest.NewRequest(http.MethodPost, path, nil),
		Body:    body,
	}
}

func run(s definition.Step, path string, body []byte) error {
	return s.Run(stepCtx(path, body))
}

func TestNewRefusesARegistryThatCannotListProviders(t *testing.T) {
	cfg, err := BuildConfig(nil)
	if err != nil {
		t.Fatalf("BuildConfig: %v", err)
	}
	if _, _, err := New(bareRegistry{}, cfg); err == nil {
		t.Fatal("expected an error for a registry that cannot list providers")
	}
}

func TestPublishAllowsAnActiveProvider(t *testing.T) {
	l := &fakeLister{ids: []string{"p1"}}
	if err := run(newStep(t, l), "/publish", publishBody("catalog/publish", "p1")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if l.calls != 0 {
		t.Fatalf("lister calls = %d, want 0: requests read the loaded list", l.calls)
	}
}

func TestPublishRejectsAnUnknownOrInactiveProvider(t *testing.T) {
	l := &fakeLister{ids: []string{"p1"}}
	err := run(newStep(t, l), "/publish", publishBody("catalog/publish", "ghost"))
	var coded *model.CodedErr
	if !errors.As(err, &coded) || coded.HTTPStatus() != http.StatusBadRequest {
		t.Fatalf("want a bad-request error, got %v", err)
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("error %q does not name the provider", err)
	}
}

func TestPublishRejectsTheWholeBatchOnOneBadProvider(t *testing.T) {
	l := &fakeLister{ids: []string{"p1"}}
	err := run(newStep(t, l), "/publish", publishBody("catalog/publish", "p1", "ghost"))
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("want an error naming ghost, got %v", err)
	}
	if l.calls != 0 {
		t.Fatalf("lister calls = %d, want 0: requests read the loaded list", l.calls)
	}
}

func TestPublishRejectsACatalogThatNamesNoProvider(t *testing.T) {
	l := &fakeLister{ids: []string{"p1"}}
	if err := run(newStep(t, l), "/publish", publishBody("catalog/publish", "p1", "")); err == nil {
		t.Fatal("expected an error for a catalog with no provider")
	}
}

func TestPublishRejectsAnEmptyCatalogList(t *testing.T) {
	l := &fakeLister{ids: []string{"p1"}}
	if err := run(newStep(t, l), "/publish", publishBody("catalog/publish")); err == nil {
		t.Fatal("expected an error for an empty catalogs list")
	}
}

func TestPublishFailsClosedWhenTheRegistryErrors(t *testing.T) {
	l := &fakeLister{err: errors.New("registry down")}
	err := run(newStep(t, l), "/publish", publishBody("catalog/publish", "p1"))
	if err == nil {
		t.Fatal("expected an error when the registry fails")
	}
	var coded *model.CodedErr
	if errors.As(err, &coded) {
		t.Fatalf("a registry outage is not a bad request, got %v", err)
	}
}

func TestBodyWithNoContextErrorsWithoutAskingTheRegistry(t *testing.T) {
	l := &fakeLister{ids: []string{"p1"}}
	if err := run(newStep(t, l), "/discover", []byte(`{"message":{}}`)); err == nil {
		t.Fatal("expected an error for a body with no context")
	}
	if l.calls != 0 {
		t.Fatalf("lister called %d times, want 0", l.calls)
	}
}

func TestPublishAcceptsTheBarePublishAction(t *testing.T) {
	l := &fakeLister{ids: []string{"p1"}}
	s := newStep(t, l)
	if err := run(s, "/publish", publishBody("publish", "p1")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := run(s, "/publish", publishBody("publish", "ghost")); err == nil {
		t.Fatal("bare publish must run the gate")
	}
}

func TestOtherActionsPassUntouched(t *testing.T) {
	l := &fakeLister{err: errors.New("registry down")}
	if err := run(newStep(t, l), "/select", []byte(`{"context":{"action":"select"},"message":{}}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if l.calls != 0 {
		t.Fatalf("lister called %d times, want 0", l.calls)
	}
}

func discoverBody(filters string) []byte {
	if filters == "" {
		return []byte(`{"context":{"action":"discover"},"message":{"intent":{"descriptor":{"name":"rice"}}}}`)
	}
	return []byte(fmt.Sprintf(`{"context":{"action":"discover"},"message":{"intent":{"filters":%s}}}`, filters))
}

func jsonpathFilter(expr string) string {
	return fmt.Sprintf(`{"type":"jsonpath","expression":%q}`, expr)
}

// filtersOf returns message.intent.filters of a discover body.
func filtersOf(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var doc struct {
		Message struct {
			Intent struct {
				Filters map[string]any `json:"filters"`
			} `json:"intent"`
		} `json:"message"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	return doc.Message.Intent.Filters
}

func TestNarrowFilterRewrites(t *testing.T) {
	both := []string{"prov-a", "prov-b"}
	allow := `@.provider.id == "prov-a" || @.provider.id == "prov-b"`
	cases := []struct {
		name    string
		filters string
		ids     []string
		want    string
	}{
		{"no filter", "", both, `$.catalogs[*] ? (` + allow + `)`},
		{"simple filter", jsonpathFilter(`$.catalogs[*] ? (@.isActive == true)`), both,
			`$.catalogs[*] ? ( (@.isActive == true) && (` + allow + `) )`},
		{"or in the consumer predicate", jsonpathFilter(`$.catalogs[*] ? (@.a == 1 || @.b == 2)`), both,
			`$.catalogs[*] ? ( (@.a == 1 || @.b == 2) && (` + allow + `) )`},
		{"paren in a string literal", jsonpathFilter(`$.catalogs[*] ? (@.name == "x)")`), both,
			`$.catalogs[*] ? ( (@.name == "x)") && (` + allow + `) )`},
		{"escaped quote in a string literal", jsonpathFilter(`$.catalogs[*] ? (@.name == "x\")")`), both,
			`$.catalogs[*] ? ( (@.name == "x\")") && (` + allow + `) )`},
		{"nested exists", jsonpathFilter(`$.catalogs[*] ? (exists(@.items[*] ? (@.price > 1)))`), both,
			`$.catalogs[*] ? ( (exists(@.items[*] ? (@.price > 1))) && (` + allow + `) )`},
		{"no ids", "", nil, `$.catalogs[*] ? (@.provider.id == "")`},
		{"no ids with filter", jsonpathFilter(`$.catalogs[*] ? (@.isActive == true)`), nil,
			`$.catalogs[*] ? ( (@.isActive == true) && (@.provider.id == "") )`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := narrowFilter(defaultField, defaultKeys, discoverBody(tc.filters), tc.ids)
			if err != nil {
				t.Fatalf("narrowFilter: %v", err)
			}
			f := filtersOf(t, out)
			if f["type"] != "jsonpath" || f["expression"] != tc.want {
				t.Fatalf("filters = %v, want jsonpath %q", f, tc.want)
			}
		})
	}
}

func TestNarrowFilterRefuses(t *testing.T) {
	good := []string{"prov-a"}
	cases := []struct {
		name string
		body []byte
		ids  []string
	}{
		{"jmespath type", discoverBody(`{"type":"jmespath","expression":"x"}`), good},
		{"not rooted at catalogs", discoverBody(jsonpathFilter(`$.items[*] ? (@.a == 1)`)), good},
		{"no predicate", discoverBody(jsonpathFilter(`$.catalogs[*]`)), good},
		{"trailing text", discoverBody(jsonpathFilter(`$.catalogs[*] ? (@.a == 1) && true`)), good},
		{"unbalanced open", discoverBody(jsonpathFilter(`$.catalogs[*] ? ((@.a == 1)`)), good},
		{"unbalanced close", discoverBody(jsonpathFilter(`$.catalogs[*] ? (@.a == 1))`)), good},
		{"unterminated string", discoverBody(jsonpathFilter(`$.catalogs[*] ? (@.a == "x)`)), good},
		{"expression not a string", discoverBody(`{"type":"jsonpath","expression":5}`), good},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := narrowFilter(defaultField, defaultKeys, tc.body, tc.ids)
			var coded *model.CodedErr
			if !errors.As(err, &coded) || coded.HTTPStatus() != http.StatusBadRequest {
				t.Fatalf("want a bad-request error, got %v (out %s)", err, out)
			}
		})
	}
}

func TestNarrowFilterKeepsOtherNumbersUnchanged(t *testing.T) {
	body := []byte(`{"context":{"action":"discover"},"message":{"big":12345678901234567890,"f":1.50,"intent":{}}}`)
	out, err := narrowFilter(defaultField, defaultKeys, body, []string{"prov-a"})
	if err != nil {
		t.Fatalf("narrowFilter: %v", err)
	}
	for _, lit := range []string{`12345678901234567890`, `1.50`} {
		if !strings.Contains(string(out), lit) {
			t.Fatalf("number %s altered in %s", lit, out)
		}
	}
	if strings.Contains(string(out), `&`) {
		t.Fatalf("output HTML-escaped: %s", out)
	}
}

func TestDiscoverNarrowsTheBody(t *testing.T) {
	l := &fakeLister{ids: []string{"prov-a"}}
	ctx := stepCtx("/discover", discoverBody(""))
	if err := newStep(t, l).Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := `$.catalogs[*] ? (@.provider.id == "prov-a")`
	if got := filtersOf(t, ctx.Body)["expression"]; got != want {
		t.Fatalf("expression = %v, want %q", got, want)
	}
}

func TestDiscoverFailsClosedWhenTheRegistryErrors(t *testing.T) {
	l := &fakeLister{err: errors.New("registry down")}
	orig := discoverBody("")
	ctx := stepCtx("/discover", orig)
	if err := newStep(t, l).Run(ctx); err == nil {
		t.Fatal("expected an error when the registry fails")
	}
	if string(ctx.Body) != string(orig) {
		t.Fatalf("body changed on a registry error: %s", ctx.Body)
	}
}

func TestNarrowFilterAcceptsTheJSONPathTypeLikeDiscoveryDoes(t *testing.T) {
	expr := `$.catalogs[*] ? (@.isActive == true)`
	want := `$.catalogs[*] ? ( (@.isActive == true) && (@.provider.id == "prov-a") )`
	for name, filters := range map[string]string{
		"absent type": fmt.Sprintf(`{"expression":%q}`, expr),
		"empty type":  fmt.Sprintf(`{"type":"","expression":%q}`, expr),
		"upper case":  fmt.Sprintf(`{"type":"JSONPATH","expression":%q}`, expr),
		"padded":      fmt.Sprintf(`{"type":" jsonpath ","expression":%q}`, expr),
	} {
		t.Run(name, func(t *testing.T) {
			out, err := narrowFilter(defaultField, defaultKeys, discoverBody(filters), []string{"prov-a"})
			if err != nil {
				t.Fatalf("narrowFilter: %v", err)
			}
			f := filtersOf(t, out)
			if f["type"] != "jsonpath" || f["expression"] != want {
				t.Fatalf("filters = %v, want jsonpath %q", f, want)
			}
		})
	}
}

func TestNarrowFilterRefusesCommentsAndSingleQuotesOutsideStrings(t *testing.T) {
	for name, expr := range map[string]string{
		"comment":              `$.catalogs[*] ? (@.a == 1 /* c */)`,
		"single quote":         `$.catalogs[*] ? (@.a == 'x')`,
		"comment hiding quote": `$.catalogs[*] ? (@.a == 1 /* " */ || @.b == 2 )`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := narrowFilter(defaultField, defaultKeys, discoverBody(jsonpathFilter(expr)), []string{"prov-a"})
			var coded *model.CodedErr
			if !errors.As(err, &coded) || coded.HTTPStatus() != http.StatusBadRequest {
				t.Fatalf("want a bad-request error, got %v", err)
			}
		})
	}
}

func TestNarrowFilterAcceptsCommentsAndSingleQuotesInsideStrings(t *testing.T) {
	for name, pred := range map[string]string{
		"single quote": `@.name == "O'Brien"`,
		"comment":      `@.note == "a /* b"`,
	} {
		t.Run(name, func(t *testing.T) {
			out, err := narrowFilter(defaultField, defaultKeys, discoverBody(jsonpathFilter(`$.catalogs[*] ? (`+pred+`)`)), []string{"prov-a"})
			if err != nil {
				t.Fatalf("narrowFilter: %v", err)
			}
			want := `$.catalogs[*] ? ( (` + pred + `) && (@.provider.id == "prov-a") )`
			if got := filtersOf(t, out)["expression"]; got != want {
				t.Fatalf("expression = %v, want %q", got, want)
			}
		})
	}
}

// wantBadRequest fails unless err is a 400.
func wantBadRequest(t *testing.T, err error) {
	t.Helper()
	var coded *model.CodedErr
	if !errors.As(err, &coded) || coded.HTTPStatus() != http.StatusBadRequest {
		t.Fatalf("want a bad-request error, got %v", err)
	}
}

func bodyWithAction(action, message string) []byte {
	if action == "<absent>" {
		return []byte(`{"context":{},"message":` + message + `}`)
	}
	return []byte(fmt.Sprintf(`{"context":{"action":%q},"message":%s}`, action, message))
}

// The router picks the target from the URL path, so the step must act on the
// path too: a body action that disagrees with it is refused, never skipped.
func TestDiscoverPathRefusesABodyActionThatDisagrees(t *testing.T) {
	for _, action := range []string{"search", "select", "publish", "", "<absent>", "dis cover"} {
		t.Run(action, func(t *testing.T) {
			l := &fakeLister{ids: []string{"prov-a"}}
			err := run(newStep(t, l), "/discover", bodyWithAction(action, `{"intent":{}}`))
			wantBadRequest(t, err)
		})
	}
}

func TestDiscoverPathNarrowsWhateverTheCaseOrPaddingOfTheAction(t *testing.T) {
	for _, path := range []string{"/discover", "/_internal/network/discover"} {
		for _, action := range []string{"discover", "Discover", " DISCOVER "} {
			t.Run(path+" "+action, func(t *testing.T) {
				l := &fakeLister{ids: []string{"prov-a"}}
				ctx := stepCtx(path, bodyWithAction(action, `{"intent":{}}`))
				if err := newStep(t, l).Run(ctx); err != nil {
					t.Fatalf("Run: %v", err)
				}
				want := `$.catalogs[*] ? (@.provider.id == "prov-a")`
				if got := filtersOf(t, ctx.Body)["expression"]; got != want {
					t.Fatalf("expression = %v, want %q", got, want)
				}
			})
		}
	}
}

func TestPublishPathRefusesABodyActionThatDisagrees(t *testing.T) {
	for _, action := range []string{"discover", "on_publish", "catalog/publishx", "", "<absent>"} {
		t.Run(action, func(t *testing.T) {
			l := &fakeLister{ids: []string{"p1"}}
			err := run(newStep(t, l), "/publish", bodyWithAction(action, `{"catalogs":[{"provider":{"id":"p1"}}]}`))
			wantBadRequest(t, err)
		})
	}
}

func TestPublishPathGatesWhateverTheCaseOfTheAction(t *testing.T) {
	for _, path := range []string{"/publish", "/_internal/network/publish"} {
		for _, action := range []string{"Publish", "catalog/Publish", " CATALOG/PUBLISH "} {
			t.Run(path+" "+action, func(t *testing.T) {
				l := &fakeLister{ids: []string{"p1"}}
				err := run(newStep(t, l), path, publishBody(action, "ghost"))
				wantBadRequest(t, err)
				if !strings.Contains(err.Error(), "ghost") {
					t.Fatalf("error %q does not name the provider", err)
				}
			})
		}
	}
}

func TestOtherPathsPassUntouchedWhateverTheAction(t *testing.T) {
	for _, action := range []string{"discover", "publish", "catalog/publish"} {
		t.Run(action, func(t *testing.T) {
			l := &fakeLister{err: errors.New("registry down")}
			body := bodyWithAction(action, `{"catalogs":[{"provider":{"id":"ghost"}}]}`)
			ctx := stepCtx("/select", body)
			if err := newStep(t, l).Run(ctx); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if l.calls != 0 || string(ctx.Body) != string(body) {
				t.Fatalf("lister calls = %d, body %s; want untouched", l.calls, ctx.Body)
			}
		})
	}
}

func TestRunWithoutARequestFailsClosed(t *testing.T) {
	l := &fakeLister{ids: []string{"p1"}}
	err := newStep(t, l).Run(&model.StepContext{Context: context.Background(), Body: publishBody("publish", "ghost")})
	if err == nil {
		t.Fatal("expected an error when the request path cannot be read")
	}
	var coded *model.CodedErr
	if errors.As(err, &coded) {
		t.Fatalf("a missing request is not the sender's fault, got %v", err)
	}
}

// PostgreSQL jsonpath reads escapes in unquoted key names, so a backslash
// outside a string can turn a quote into a different token than the scanner
// thinks. Refused rather than modelled.
func TestNarrowFilterRefusesABackslashOutsideStrings(t *testing.T) {
	for name, expr := range map[string]string{
		"escaped quote in a key": `$.catalogs[*] ? (@.k\" == 1) || 1 == 1 || (@.b == "zz\"ww")`,
		"escaped paren in a key": `$.catalogs[*] ? (@.k\( == 1) || 1 == 1 || (@.b == 2))`,
		"lone backslash":         `$.catalogs[*] ? (@.a\x == 1)`,
	} {
		t.Run(name, func(t *testing.T) {
			out, err := narrowFilter(defaultField, defaultKeys, discoverBody(jsonpathFilter(expr)), []string{"prov-a"})
			var coded *model.CodedErr
			if !errors.As(err, &coded) || coded.HTTPStatus() != http.StatusBadRequest {
				t.Fatalf("want a bad-request error, got %v (out %s)", err, out)
			}
		})
	}
}

func TestNarrowFilterAcceptsABackslashInsideAString(t *testing.T) {
	pred := `@.name == "a\"b"`
	out, err := narrowFilter(defaultField, defaultKeys, discoverBody(jsonpathFilter(`$.catalogs[*] ? (`+pred+`)`)), []string{"prov-a"})
	if err != nil {
		t.Fatalf("narrowFilter: %v", err)
	}
	want := `$.catalogs[*] ? ( (` + pred + `) && (@.provider.id == "prov-a") )`
	if got := filtersOf(t, out)["expression"]; got != want {
		t.Fatalf("expression = %v, want %q", got, want)
	}
}

// Go's struct decoding matches keys by case and Unicode fold, last key wins,
// while other readers match exactly. A body holding two spellings of a field
// this step reads is refused, so every reader downstream sees the same field.
// longS is LATIN SMALL LETTER LONG S, which Go's decoder folds to "s". (The
// Kelvin sign folds to "k", but no key this step reads contains a "k".)
var longS = string(rune(0x17F))

func TestPublishRefusesKeysThatDifferOnlyByFold(t *testing.T) {
	cases := map[string]string{
		"upper-case id beside id":      `{"context":{"action":"publish"},"message":{"catalogs":[{"provider":{"id":"evil","ID":"p1"}}]}}`,
		"folded catalogs beside":       `{"context":{"action":"publish"},"message":{"catalogs":[{"provider":{"id":"evil"}}],"catalog` + longS + `":[{"provider":{"id":"p1"}}]}}`,
		"capital Catalogs beside":      `{"context":{"action":"publish"},"message":{"catalogs":[{"provider":{"id":"evil"}}],"Catalogs":[{"provider":{"id":"p1"}}]}}`,
		"only a capital Catalogs":      `{"context":{"action":"publish"},"message":{"Catalogs":[{"provider":{"id":"p1"}}]}}`,
		"capital Provider":             `{"context":{"action":"publish"},"message":{"catalogs":[{"provider":{"id":"p1"},"Provider":{"id":"evil"}}]}}`,
		"capital iD in a catalog":      `{"context":{"action":"publish"},"message":{"catalogs":[{"id":"c","iD":"d","provider":{"id":"p1"}}]}}`,
		"capital Message at the root":  `{"context":{"action":"publish"},"message":{"catalogs":[{"provider":{"id":"p1"}}]},"Message":{}}`,
		"folded message at the root":   `{"context":{"action":"publish"},"message":{"catalogs":[{"provider":{"id":"p1"}}]},"me` + longS + longS + `age":{}}`,
		"capital Context at the root":  `{"context":{"action":"publish"},"Context":{"action":"x"},"message":{"catalogs":[{"provider":{"id":"p1"}}]}}`,
		"capital Action in context":    `{"context":{"action":"publish","Action":"x"},"message":{"catalogs":[{"provider":{"id":"p1"}}]}}`,
		"exact duplicate provider id":  `{"context":{"action":"publish"},"message":{"catalogs":[{"provider":{"id":"evil","id":"p1"}}]}}`,
		"exact duplicate catalogs key": `{"context":{"action":"publish"},"message":{"catalogs":[{"provider":{"id":"evil"}}],"catalogs":[{"provider":{"id":"p1"}}]}}`,
		"exact duplicate deep down":    `{"context":{"action":"publish"},"message":{"catalogs":[{"provider":{"id":"p1"},"items":[{"price":1,"price":2}]}]}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			l := &fakeLister{ids: []string{"p1"}}
			wantBadRequest(t, run(newStep(t, l), "/publish", []byte(body)))
		})
	}
}

func TestPublishStillAcceptsAPlainBody(t *testing.T) {
	l := &fakeLister{ids: []string{"p1"}}
	body := `{"context":{"action":"publish"},"message":{"catalogs":[{"id":"c1","descriptor":{"name":"x"},"provider":{"id":"p1","descriptor":{"name":"y"}}}]}}`
	if err := run(newStep(t, l), "/publish", []byte(body)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDiscoverRefusesKeysThatDifferOnlyByFold(t *testing.T) {
	expr := `"$.catalogs[*] ? (@.a == 1)"`
	cases := map[string]string{
		"folded filters beside":  `{"context":{"action":"discover"},"message":{"intent":{"filters":{"type":"jsonpath","expression":` + expr + `},"filter` + longS + `":{"type":"jsonpath","expression":"$.catalogs[*]"}}}}`,
		"only a capital Filters": `{"context":{"action":"discover"},"message":{"intent":{"Filters":{"type":"jsonpath","expression":"$.catalogs[*]"}}}}`,
		"capital Intent":         `{"context":{"action":"discover"},"message":{"intent":{},"Intent":{"filters":null}}}`,
		"capital Message":        `{"context":{"action":"discover"},"MESSAGE":{"intent":{}}}`,
		"capital Expression":     `{"context":{"action":"discover"},"message":{"intent":{"filters":{"type":"jsonpath","expression":` + expr + `,"Expression":"$.catalogs[*]"}}}}`,
		"capital Type":           `{"context":{"action":"discover"},"message":{"intent":{"filters":{"type":"jsonpath","TYPE":"jmespath","expression":` + expr + `}}}}`,
		"capital Action":         `{"context":{"action":"discover","ACTION":"search"},"message":{"intent":{}}}`,
		"folded expression":      `{"context":{"action":"discover"},"message":{"intent":{"filters":{"type":"jsonpath","expression":` + expr + `,"expre` + longS + `sion":"$.catalogs[*]"}}}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			l := &fakeLister{ids: []string{"prov-a"}}
			wantBadRequest(t, run(newStep(t, l), "/discover", []byte(body)))
		})
	}
}

func TestDiscoverKeepsSiblingsAndHandlesAbsentParts(t *testing.T) {
	allow := `@.provider.id == "prov-a"`
	cases := []struct {
		name, body, want string
		keep             []string
	}{
		{"siblings survive",
			`{"context":{"action":"discover"},"message":{"intent":{"descriptor":{"name":"rice"},"spatial":[{"op":"s_dwithin"}],"textSearch":"rice","schemaContext":["a"],"filters":{"type":"jsonpath","expression":"$.catalogs[*] ? (@.a == 1)"}}}}`,
			`$.catalogs[*] ? ( (@.a == 1) && (` + allow + `) )`,
			[]string{`"descriptor":{"name":"rice"}`, `"spatial":[{"op":"s_dwithin"}]`, `"textSearch":"rice"`, `"schemaContext":["a"]`}},
		{"message absent", `{"context":{"action":"discover"}}`, `$.catalogs[*] ? (` + allow + `)`, nil},
		{"intent absent", `{"context":{"action":"discover"},"message":{}}`, `$.catalogs[*] ? (` + allow + `)`, nil},
		{"filters null", `{"context":{"action":"discover"},"message":{"intent":{"filters":null}}}`, `$.catalogs[*] ? (` + allow + `)`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := stepCtx("/discover", []byte(tc.body))
			if err := newStep(t, &fakeLister{ids: []string{"prov-a"}}).Run(ctx); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got := filtersOf(t, ctx.Body)["expression"]; got != tc.want {
				t.Fatalf("expression = %v, want %q", got, tc.want)
			}
			for _, k := range tc.keep {
				if !strings.Contains(string(ctx.Body), k) {
					t.Fatalf("%s lost from %s", k, ctx.Body)
				}
			}
		})
	}
}

// A bad id in the registry is the registry's fault, not the sender's: a plain
// error (500), never a 400.
func TestNarrowFilterFailsPlainlyOnABadRegistryID(t *testing.T) {
	for name, ids := range map[string][]string{
		"id with a quote": {`a"b`},
		"id too short":    {"ab"},
		"empty id":        {""},
		"one bad of two":  {"prov-a", "x"},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := narrowFilter(defaultField, defaultKeys, discoverBody(""), ids)
			if err == nil {
				t.Fatalf("expected an error, got %s", out)
			}
			var coded *model.CodedErr
			if errors.As(err, &coded) {
				t.Fatalf("a bad registry id is not a bad request, got %v", err)
			}
		})
	}
}

// defaultField is the provider id path the step reads when not configured.
var defaultField = []string{"provider", "id"}

// defaultKeys are the keys the step reads when not configured.
var defaultKeys = append(slices.Clone(fixedKeys), defaultField...)

func TestBuildConfigDefaultsReproduceTheFixedBehaviour(t *testing.T) {
	want := Config{
		PublishEndpoint:  "publish",
		DiscoverEndpoint: "discover",
		ProviderField:    "provider.id",
		RefreshInterval:  10 * time.Second,
		MaxStaleness:     60 * time.Second,
	}
	for name, config := range map[string]map[string]string{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			got, err := BuildConfig(config)
			if err != nil {
				t.Fatalf("BuildConfig: %v", err)
			}
			if got != want {
				t.Fatalf("config = %+v, want %+v", got, want)
			}
		})
	}
}

func TestBuildConfigReadsEveryKey(t *testing.T) {
	got, err := BuildConfig(map[string]string{
		"publishEndpoint":  "submit",
		"discoverEndpoint": "search",
		"providerField":    "bpp.provider.id",
		"participantType":  "node",
		"participantRole":  "provider",
		"refreshInterval":  "5s",
		"maxStaleness":     "30s",
	})
	if err != nil {
		t.Fatalf("BuildConfig: %v", err)
	}
	want := Config{"submit", "search", "bpp.provider.id", "node", "provider", 5 * time.Second, 30 * time.Second}
	if got != want {
		t.Fatalf("config = %+v, want %+v", got, want)
	}
}

// A bad value fails the plugin at startup. A typo must never fall back to a
// default on a security check, and providerField is spliced into a jsonpath.
func TestBuildConfigRefuses(t *testing.T) {
	for name, config := range map[string]map[string]string{
		"unknown key":                {"providerfield": "provider.id"},
		"interval under 1s":          {"refreshInterval": "500ms"},
		"interval not a duration":    {"refreshInterval": "x"},
		"staleness under interval":   {"refreshInterval": "10s", "maxStaleness": "5s"},
		"typo of a key":              {"participantRoles": "provider"},
		"empty publish endpoint":     {"publishEndpoint": ""},
		"upper-case publish":         {"publishEndpoint": "Publish"},
		"publish with a slash":       {"publishEndpoint": "catalog/publish"},
		"publish starting digit":     {"publishEndpoint": "1publish"},
		"discover with a space":      {"discoverEndpoint": "dis cover"},
		"discover with a dash":       {"discoverEndpoint": "dis-cover"},
		"same endpoints":             {"publishEndpoint": "x", "discoverEndpoint": "x"},
		"publish equals the default": {"publishEndpoint": "discover"},
		"empty provider field":       {"providerField": ""},
		"provider field quote":       {"providerField": `provider.id" || true || "`},
		"provider field space":       {"providerField": "provider id"},
		"provider field leading dot": {"providerField": ".provider.id"},
		"provider field double dot":  {"providerField": "provider..id"},
		"provider field trailing":    {"providerField": "provider.id."},
		"provider field digit first": {"providerField": "provider.1id"},
		"provider field wildcard":    {"providerField": "provider[*].id"},
		"provider field dollar":      {"providerField": "$.provider.id"},
		"provider field non-ascii":   {"providerField": "provider.\u0131d"},
		"unknown participant type":   {"participantType": "bpp"},
		"upper-case type":            {"participantType": "Node"},
		"unknown participant role":   {"participantRole": "admin"},
		"padded role":                {"participantRole": " provider"},
	} {
		t.Run(name, func(t *testing.T) {
			if cfg, err := BuildConfig(config); err == nil {
				t.Fatalf("BuildConfig(%v) = %+v, want an error", config, cfg)
			}
		})
	}
}

func TestBuildConfigAcceptsEveryAllowedTypeAndRole(t *testing.T) {
	for _, typ := range []string{"", "node", "upstream"} {
		for _, role := range []string{"", "consumer", "provider", "network"} {
			if _, err := BuildConfig(map[string]string{"participantType": typ, "participantRole": role}); err != nil {
				t.Errorf("BuildConfig(type %q, role %q): %v", typ, role, err)
			}
		}
	}
}

// New checks the config itself, so a Config built by hand cannot skip the
// validation that keeps providerField out of the jsonpath grammar.
func TestNewRefusesAnInvalidConfig(t *testing.T) {
	cfg, err := BuildConfig(nil)
	if err != nil {
		t.Fatalf("BuildConfig: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"zero config":    func(c *Config) { *c = Config{} },
		"bad field":      func(c *Config) { c.ProviderField = `id" || true` },
		"bad endpoint":   func(c *Config) { c.PublishEndpoint = "a/b" },
		"same endpoints": func(c *Config) { c.DiscoverEndpoint = c.PublishEndpoint },
		"bad role":       func(c *Config) { c.ParticipantRole = "admin" },
		"bad type":       func(c *Config) { c.ParticipantType = "bpp" },
		"no interval":    func(c *Config) { c.RefreshInterval = 0 },
		"stale < tick":   func(c *Config) { c.MaxStaleness = c.RefreshInterval / 2 },
	} {
		t.Run(name, func(t *testing.T) {
			bad := cfg
			mutate(&bad)
			if _, _, err := New(listingRegistry{fakeLister: &fakeLister{}}, bad); err == nil {
				t.Fatalf("New(%+v) succeeded, want an error", bad)
			}
		})
	}
}

func TestConfiguredPublishEndpointIsGated(t *testing.T) {
	config := map[string]string{"publishEndpoint": "submit"}
	for _, path := range []string{"/submit", "/_internal/network/submit"} {
		for _, action := range []string{"submit", "catalog/submit", " Catalog/Submit "} {
			t.Run(path+" "+action, func(t *testing.T) {
				l := &fakeLister{ids: []string{"p1"}}
				s := newStepWith(t, l, config)
				if err := run(s, path, publishBody(action, "p1")); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				err := run(s, path, publishBody(action, "ghost"))
				wantBadRequest(t, err)
				if !strings.Contains(err.Error(), "ghost") {
					t.Fatalf("error %q does not name the provider", err)
				}
			})
		}
	}
}

func TestConfiguredPublishEndpointRefusesTheOldActions(t *testing.T) {
	for _, action := range []string{"publish", "catalog/publish", "", "<absent>"} {
		t.Run(action, func(t *testing.T) {
			l := &fakeLister{ids: []string{"p1"}}
			s := newStepWith(t, l, map[string]string{"publishEndpoint": "submit"})
			wantBadRequest(t, run(s, "/submit", bodyWithAction(action, `{"catalogs":[{"provider":{"id":"p1"}}]}`)))
		})
	}
}

func TestConfiguredEndpointsLeaveTheDefaultPathsUntouched(t *testing.T) {
	config := map[string]string{"publishEndpoint": "submit", "discoverEndpoint": "search"}
	for path, action := range map[string]string{"/publish": "publish", "/discover": "discover"} {
		t.Run(path, func(t *testing.T) {
			l := &fakeLister{err: errors.New("registry down")}
			body := bodyWithAction(action, `{"catalogs":[{"provider":{"id":"ghost"}}]}`)
			ctx := stepCtx(path, body)
			if err := newStepWith(t, l, config).Run(ctx); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if l.calls != 0 || string(ctx.Body) != string(body) {
				t.Fatalf("lister calls = %d, body %s; want untouched", l.calls, ctx.Body)
			}
		})
	}
}

func TestConfiguredDiscoverEndpointIsNarrowed(t *testing.T) {
	config := map[string]string{"discoverEndpoint": "search"}
	for _, path := range []string{"/search", "/_internal/network/search"} {
		t.Run(path, func(t *testing.T) {
			l := &fakeLister{ids: []string{"prov-a"}}
			ctx := stepCtx(path, bodyWithAction("Search", `{"intent":{}}`))
			if err := newStepWith(t, l, config).Run(ctx); err != nil {
				t.Fatalf("Run: %v", err)
			}
			want := `$.catalogs[*] ? (@.provider.id == "prov-a")`
			if got := filtersOf(t, ctx.Body)["expression"]; got != want {
				t.Fatalf("expression = %v, want %q", got, want)
			}
		})
	}
	for _, action := range []string{"discover", "catalog/search", ""} {
		t.Run("refuses "+action, func(t *testing.T) {
			l := &fakeLister{ids: []string{"prov-a"}}
			wantBadRequest(t, run(newStepWith(t, l, config), "/search", bodyWithAction(action, `{"intent":{}}`)))
		})
	}
}

// bppPublish is a publish whose one catalog carries the given JSON under the
// catalog itself.
func bppPublish(catalog string) []byte {
	return []byte(`{"context":{"action":"publish"},"message":{"catalogs":[` + catalog + `]}}`)
}

func TestConfiguredProviderFieldOnPublish(t *testing.T) {
	config := map[string]string{"providerField": "bpp.provider.id"}
	l := &fakeLister{ids: []string{"p1"}}
	s := newStepWith(t, l, config)
	if err := run(s, "/publish", bppPublish(`{"bpp":{"provider":{"id":"p1"}}}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	err := run(s, "/publish", bppPublish(`{"bpp":{"provider":{"id":"ghost"}},"provider":{"id":"p1"}}`))
	wantBadRequest(t, err)
	if !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("error %q does not name the provider", err)
	}
	for name, catalog := range map[string]string{
		"only the old field":  `{"provider":{"id":"p1"}}`,
		"bpp not an object":   `{"bpp":"p1"}`,
		"provider not object": `{"bpp":{"provider":"p1"}}`,
		"id not a string":     `{"bpp":{"provider":{"id":5}}}`,
		"empty id":            `{"bpp":{"provider":{"id":""}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			wantBadRequest(t, run(s, "/publish", bppPublish(catalog)))
		})
	}
}

// The exact-key rule holds on every object along a configured path, not only
// on the names the step used to hard-code.
func TestConfiguredProviderFieldRefusesLookalikeKeys(t *testing.T) {
	config := map[string]string{"providerField": "bpp.provider.id"}
	for name, catalog := range map[string]string{
		"capital Bpp beside":      `{"bpp":{"provider":{"id":"p1"}},"Bpp":{"provider":{"id":"evil"}}}`,
		"only a capital BPP":      `{"BPP":{"provider":{"id":"p1"}}}`,
		"capital Provider inside": `{"bpp":{"provider":{"id":"p1"},"Provider":{"id":"evil"}}}`,
		"upper-case PROVIDER":     `{"bpp":{"provider":{"id":"p1"},"PROVIDER":{"id":"evil"}}}`,
		"capital ID innermost":    `{"bpp":{"provider":{"id":"p1","ID":"evil"}}}`,
		"duplicate id innermost":  `{"bpp":{"provider":{"id":"evil","id":"p1"}}}`,
		"duplicate bpp":           `{"bpp":{"provider":{"id":"evil"}},"bpp":{"provider":{"id":"p1"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			l := &fakeLister{ids: []string{"p1"}}
			wantBadRequest(t, run(newStepWith(t, l, config), "/publish", bppPublish(catalog)))
		})
	}
}

// longS folds to "s", so a configured segment holding an "s" has a Unicode
// lookalike, refused like the fixed keys' ones.
func TestConfiguredProviderFieldRefusesUnicodeFoldedKeys(t *testing.T) {
	config := map[string]string{"providerField": "seller.ids"}
	for name, catalog := range map[string]string{
		"folded outer": `{"seller":{"ids":"p1"},"` + longS + `eller":{"ids":"evil"}}`,
		"folded inner": `{"seller":{"ids":"p1","id` + longS + `":"evil"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			l := &fakeLister{ids: []string{"p1"}}
			wantBadRequest(t, run(newStepWith(t, l, config), "/publish", bppPublish(catalog)))
		})
	}
	l := &fakeLister{ids: []string{"p1"}}
	if err := run(newStepWith(t, l, config), "/publish", bppPublish(`{"seller":{"ids":"p1"}}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestConfiguredProviderFieldOnDiscover(t *testing.T) {
	config := map[string]string{"providerField": "bpp.provider.id"}
	for _, tc := range []struct {
		name, filters string
		ids           []string
		want          string
	}{
		{"no filter", "", []string{"prov-a", "prov-b"},
			`$.catalogs[*] ? (@.bpp.provider.id == "prov-a" || @.bpp.provider.id == "prov-b")`},
		{"with a filter", jsonpathFilter(`$.catalogs[*] ? (@.a == 1)`), []string{"prov-a"},
			`$.catalogs[*] ? ( (@.a == 1) && (@.bpp.provider.id == "prov-a") )`},
		{"no ids", "", nil, `$.catalogs[*] ? (@.bpp.provider.id == "")`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := stepCtx("/discover", discoverBody(tc.filters))
			if err := newStepWith(t, &fakeLister{ids: tc.ids}, config).Run(ctx); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got := filtersOf(t, ctx.Body)["expression"]; got != tc.want {
				t.Fatalf("expression = %v, want %q", got, tc.want)
			}
		})
	}
}

func TestConfiguredTypeAndRoleReachTheLister(t *testing.T) {
	for _, tc := range []struct {
		config    map[string]string
		typ, role string
	}{
		{nil, "", ""},
		{map[string]string{"participantType": "node", "participantRole": "provider"}, "node", "provider"},
		{map[string]string{"participantType": "upstream"}, "upstream", ""},
		{map[string]string{"participantRole": "network"}, "", "network"},
	} {
		for path, body := range map[string][]byte{
			"/publish":  publishBody("publish", "prov-1"),
			"/discover": discoverBody(""),
		} {
			t.Run(fmt.Sprint(tc.config, path), func(t *testing.T) {
				l := &fakeLister{ids: []string{"prov-1"}}
				if err := run(newStepWith(t, l, tc.config), path, body); err != nil {
					t.Fatalf("Run: %v", err)
				}
				// The restriction is sent when the list is loaded; requests
				// read the loaded list and make no call of their own.
				if l.calls != 0 || l.participantType != tc.typ || l.role != tc.role {
					t.Fatalf("calls = %d, type %q, role %q; want 0, %q, %q",
						l.calls, l.participantType, l.role, tc.typ, tc.role)
				}
			})
		}
	}
}

// Crossed endpoints would gate the router's discover and leave its publish
// open, so the router's real names may not be swapped.
func TestBuildConfigRefusesCrossedEndpoints(t *testing.T) {
	for name, config := range map[string]map[string]string{
		"swapped":             {"publishEndpoint": "discover", "discoverEndpoint": "publish"},
		"discover on publish": {"discoverEndpoint": "publish", "publishEndpoint": "submit"},
		"publish on discover": {"publishEndpoint": "discover", "discoverEndpoint": "search"},
	} {
		t.Run(name, func(t *testing.T) {
			if cfg, err := BuildConfig(config); err == nil {
				t.Fatalf("BuildConfig(%v) = %+v, want an error", config, cfg)
			}
		})
	}
}

// A field segment that folds onto another read key, without being it, would
// make every request a 400; it is refused at startup instead.
func TestBuildConfigRefusesFoldedFieldSegments(t *testing.T) {
	for _, field := range []string{"Message.id", "Type", "ID.id", "bpp.Catalogs.id", "a.Action"} {
		t.Run(field, func(t *testing.T) {
			if cfg, err := BuildConfig(map[string]string{"providerField": field}); err == nil {
				t.Fatalf("BuildConfig(providerField %q) = %+v, want an error", field, cfg)
			}
		})
	}
	for _, field := range []string{"provider.id", "bpp.provider.id", "message.id", "provider.provider"} {
		t.Run("accepts "+field, func(t *testing.T) {
			if _, err := BuildConfig(map[string]string{"providerField": field}); err != nil {
				t.Fatalf("BuildConfig(providerField %q): %v", field, err)
			}
		})
	}
}

// The path decides before the body is read, so a bodyless or non-Beckn
// request on a path the step does not gate passes untouched.
func TestUngatedPathsPassWithoutAContext(t *testing.T) {
	for _, tc := range []struct {
		name, method, path string
		body               []byte
	}{
		{"bodyless GET", http.MethodGet, "/catalog/subscription", nil},
		{"DELETE with an empty body", http.MethodDelete, "/catalog/subscription", []byte{}},
		{"non-JSON body", http.MethodPost, "/select", []byte("not json")},
		{"JSON without context", http.MethodPost, "/select", []byte(`{"message":{}}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &fakeLister{err: errors.New("registry down")}
			ctx := &model.StepContext{
				Context: context.Background(),
				Request: httptest.NewRequest(tc.method, tc.path, nil),
				Body:    tc.body,
			}
			if err := newStep(t, l).Run(ctx); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if l.calls != 0 || string(ctx.Body) != string(tc.body) {
				t.Fatalf("lister calls = %d, body %q; want untouched", l.calls, ctx.Body)
			}
		})
	}
}

func TestGatedPathsStillRefuseABodyWithoutAContext(t *testing.T) {
	for _, path := range []string{"/publish", "/discover", "/_internal/network/publish"} {
		for name, body := range map[string][]byte{"no body": nil, "non-JSON": []byte("x"), "no context": []byte(`{"message":{}}`)} {
			t.Run(path+" "+name, func(t *testing.T) {
				l := &fakeLister{ids: []string{"p1"}}
				wantBadRequest(t, run(newStep(t, l), path, body))
				if l.calls != 0 {
					t.Fatalf("lister called %d times, want 0", l.calls)
				}
			})
		}
	}
}

// Discovery reads only type and expression, so the rewritten filters carry
// those two and nothing else: any other member the consumer sent is dropped.
func TestNarrowFilterDropsOtherFilterMembers(t *testing.T) {
	for name, filters := range map[string]string{
		"with type":    `{"type":"jsonpath","expression":"$.catalogs[*] ? (@.a == 1)","extra":"x","limits":{"n":5}}`,
		"without type": `{"expression":"$.catalogs[*] ? (@.a == 1)","extra":"x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			out, err := narrowFilter(defaultField, defaultKeys, discoverBody(filters), []string{"prov-a"})
			if err != nil {
				t.Fatalf("narrowFilter: %v", err)
			}
			f := filtersOf(t, out)
			want := `$.catalogs[*] ? ( (@.a == 1) && (@.provider.id == "prov-a") )`
			if len(f) != 2 || f["type"] != "jsonpath" || f["expression"] != want {
				t.Fatalf("filters = %v, want only type jsonpath and expression %q", f, want)
			}
		})
	}
}

func TestPublishNamesTheFirstBadProviderInCatalogOrder(t *testing.T) {
	l := &fakeLister{ids: []string{"p1", "p2"}}
	err := run(newStep(t, l), "/publish", publishBody("publish", "p1", "ghost-b", "p2", "ghost-a"))
	wantBadRequest(t, err)
	if !strings.Contains(err.Error(), `"ghost-b"`) || strings.Contains(err.Error(), "ghost-a") {
		t.Fatalf("error %q, want it to name ghost-b only", err)
	}
}
