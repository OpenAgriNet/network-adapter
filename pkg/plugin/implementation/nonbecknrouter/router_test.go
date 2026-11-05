package nonbecknrouter

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const turnsRule = `
routingRules:
  - targetType: url
    target:
      url: http://dss:8000
    endpoints:
      - v1/turns
`

func tryRouterFrom(t *testing.T, rules string) (*Router, error) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "rules.yaml")
	if err := os.WriteFile(file, []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}
	r, _, err := New(context.Background(), &Config{RoutingConfig: file})
	return r, err
}

func newRouterFrom(t *testing.T, rules string) *Router {
	t.Helper()
	r, err := tryRouterFrom(t, rules)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func routeTo(t *testing.T, r *Router, path, query string, body []byte) string {
	t.Helper()
	route, err := r.Route(context.Background(), &url.URL{Path: path, RawQuery: query}, body)
	if err != nil {
		t.Fatalf("Route(%q): %v", path, err)
	}
	if route.TargetType != "url" {
		t.Fatalf("target type = %q, want url", route.TargetType)
	}
	return route.URL.String()
}

func wantRoute(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestRouteSendsAnEndpointToItsTarget(t *testing.T) {
	r := newRouterFrom(t, turnsRule)
	wantRoute(t, routeTo(t, r, "v1/turns", "", nil), "http://dss:8000/v1/turns")
}

func TestRouteSendsEachEndpointToItsOwnTarget(t *testing.T) {
	rules := turnsRule + `
  - targetType: url
    target:
      url: http://other:9000
    endpoints:
      - v1/files
`
	r := newRouterFrom(t, rules)
	wantRoute(t, routeTo(t, r, "v1/turns", "", nil), "http://dss:8000/v1/turns")
	wantRoute(t, routeTo(t, r, "v1/files", "", nil), "http://other:9000/v1/files")
}

func TestRouteMatchesAPathWithALeadingSlash(t *testing.T) {
	r := newRouterFrom(t, turnsRule)
	wantRoute(t, routeTo(t, r, "/v1/turns", "", nil), "http://dss:8000/v1/turns")
}

func TestRouteKeepsThePathTheTargetAlreadyHas(t *testing.T) {
	r := newRouterFrom(t, strings.Replace(turnsRule, "http://dss:8000", "http://dss:8000/api", 1))
	wantRoute(t, routeTo(t, r, "v1/turns", "", nil), "http://dss:8000/api/v1/turns")
}

func TestRouteLeavesTheTargetPathAloneWhenExcludeActionIsSet(t *testing.T) {
	rules := `
routingRules:
  - targetType: url
    target:
      url: http://dss:8000/api/all
      excludeAction: true
    endpoints:
      - v1/turns
`
	r := newRouterFrom(t, rules)
	wantRoute(t, routeTo(t, r, "v1/turns", "", nil), "http://dss:8000/api/all")
}

func TestRouteForwardsTheQueryString(t *testing.T) {
	r := newRouterFrom(t, turnsRule)
	wantRoute(t, routeTo(t, r, "v1/turns", "stream=1&lang=hi", nil), "http://dss:8000/v1/turns?stream=1&lang=hi")
}

func TestRouteDoesNotLetALaterRequestChangeAnEarlierRoute(t *testing.T) {
	r := newRouterFrom(t, turnsRule)
	first, err := r.Route(context.Background(), &url.URL{Path: "v1/turns", RawQuery: "a=1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	routeTo(t, r, "v1/turns", "b=2", nil)
	wantRoute(t, first.URL.String(), "http://dss:8000/v1/turns?a=1")
}

func TestRouteIgnoresTheBody(t *testing.T) {
	r := newRouterFrom(t, turnsRule)
	bodies := map[string][]byte{
		"no body":               nil,
		"plain text":            []byte("hello"),
		"json without context":  []byte(`{"query":"tomato leaf curl"}`),
		"json with bad context": []byte(`{"context":"x"}`),
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			wantRoute(t, routeTo(t, r, "v1/turns", "", body), "http://dss:8000/v1/turns")
		})
	}
}

func TestRouteRefusesAnEndpointNoRuleNames(t *testing.T) {
	r := newRouterFrom(t, turnsRule)
	_, err := r.Route(context.Background(), &url.URL{Path: "v1/other"}, nil)
	if err == nil || !strings.Contains(err.Error(), "v1/other") {
		t.Errorf("expected an error naming v1/other, got %v", err)
	}
}

func TestRouteRefusesANilRequestURL(t *testing.T) {
	r := newRouterFrom(t, turnsRule)
	if _, err := r.Route(context.Background(), nil, nil); err == nil {
		t.Error("expected an error for a nil request URL")
	}
}

func TestNewRefusesABadRoutingFile(t *testing.T) {
	cases := map[string]string{
		"not yaml":              "routingRules: [",
		"no rules":              "routingRules: []",
		"unknown target type":   strings.Replace(turnsRule, "targetType: url", "targetType: bpp", 1),
		"publisher target":      strings.Replace(turnsRule, "targetType: url", "targetType: publisher", 1),
		"no target url":         strings.Replace(turnsRule, "url: http://dss:8000", "url: \"\"", 1),
		"target without scheme": strings.Replace(turnsRule, "http://dss:8000", "dss:8000", 1),
		"target not http":       strings.Replace(turnsRule, "http://dss:8000", "ftp://dss:8000", 1),
		"target without host":   strings.Replace(turnsRule, "http://dss:8000", "http://", 1),
		"no endpoints":          strings.Replace(turnsRule, "      - v1/turns", "", 1),
		"blank endpoint":        strings.Replace(turnsRule, "v1/turns", "\"  \"", 1),
		"endpoint listed twice": strings.Replace(turnsRule, "      - v1/turns", "      - v1/turns\n      - /v1/turns", 1),
		"endpoint in two rules": turnsRule + strings.TrimPrefix(turnsRule, "\nroutingRules:\n"),
	}
	for name, rules := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := tryRouterFrom(t, rules); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestNewRefusesAMissingRoutingFile(t *testing.T) {
	cases := map[string]*Config{
		"nil config":     nil,
		"empty path":     {RoutingConfig: ""},
		"file not found": {RoutingConfig: filepath.Join(t.TempDir(), "absent.yaml")},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := New(context.Background(), cfg); err == nil {
				t.Error("expected an error")
			}
		})
	}
}
