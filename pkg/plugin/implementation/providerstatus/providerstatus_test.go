package providerstatus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

type fakeLister struct {
	ids   []string
	err   error
	calls int
}

func (f *fakeLister) ActiveProviderIDs(ctx context.Context) ([]string, error) {
	f.calls++
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
	s, err := New(listingRegistry{fakeLister: l})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
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
	if _, err := New(bareRegistry{}); err == nil {
		t.Fatal("expected an error for a registry that cannot list providers")
	}
}

func TestPublishAllowsAnActiveProvider(t *testing.T) {
	l := &fakeLister{ids: []string{"p1"}}
	if err := run(newStep(t, l), "/publish", publishBody("catalog/publish", "p1")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if l.calls != 1 {
		t.Fatalf("lister calls = %d, want 1", l.calls)
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
	if l.calls != 1 {
		t.Fatalf("lister calls = %d, want 1", l.calls)
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
			out, err := narrowFilter(discoverBody(tc.filters), tc.ids)
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
		{"id with a quote", discoverBody(""), []string{`a"b`}},
		{"id too short", discoverBody(""), []string{"ab"}},
		{"empty id", discoverBody(""), []string{""}},
		{"expression not a string", discoverBody(`{"type":"jsonpath","expression":5}`), good},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := narrowFilter(tc.body, tc.ids)
			var coded *model.CodedErr
			if !errors.As(err, &coded) || coded.HTTPStatus() != http.StatusBadRequest {
				t.Fatalf("want a bad-request error, got %v (out %s)", err, out)
			}
		})
	}
}

func TestNarrowFilterKeepsOtherNumbersUnchanged(t *testing.T) {
	body := []byte(`{"context":{"action":"discover"},"message":{"big":12345678901234567890,"f":1.50,"intent":{}}}`)
	out, err := narrowFilter(body, []string{"prov-a"})
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
			out, err := narrowFilter(discoverBody(filters), []string{"prov-a"})
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
			_, err := narrowFilter(discoverBody(jsonpathFilter(expr)), []string{"prov-a"})
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
			out, err := narrowFilter(discoverBody(jsonpathFilter(`$.catalogs[*] ? (`+pred+`)`)), []string{"prov-a"})
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
