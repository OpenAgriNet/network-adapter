package main

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

const rules = `
routingRules:
  - targetType: url
    target:
      url: http://dss:8000
    endpoints:
      - v1/turns
`

func writeRules(t *testing.T, content string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "rules.yaml")
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestProviderBuildsARouterFromTheRoutingFile(t *testing.T) {
	config := map[string]string{"routingConfig": writeRules(t, rules)}
	router, _, err := RouterProvider{}.New(context.Background(), config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	route, err := router.Route(context.Background(), &url.URL{Path: "v1/turns"}, []byte("not beckn"))
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if got, want := route.URL.String(), "http://dss:8000/v1/turns"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestProviderRefusesABadConfig(t *testing.T) {
	cases := map[string]map[string]string{
		"no config":             nil,
		"routingConfig missing": {"other": "x"},
		"routingConfig empty":   {"routingConfig": ""},
		"file not found":        {"routingConfig": filepath.Join(t.TempDir(), "absent.yaml")},
		"file has no rules":     {"routingConfig": writeRules(t, "routingRules: []")},
	}
	for name, config := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := (RouterProvider{}).New(context.Background(), config); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestProviderRefusesANilContext(t *testing.T) {
	config := map[string]string{"routingConfig": writeRules(t, rules)}
	//nolint:staticcheck
	if _, _, err := (RouterProvider{}).New(nil, config); err == nil {
		t.Error("expected an error")
	}
}
