package util

import (
	"errors"
	"net/http"
	"net/url"
	"testing"
)

func redirectHop(t *testing.T, raw string) *http.Request {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return &http.Request{URL: u}
}

func TestRefuseOffHostRedirect(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		from, to string
		refused  bool
	}{
		"same host, new path":        {from: "https://api.example/a", to: "https://api.example/b", refused: false},
		"host case differs":          {from: "https://API.example/a", to: "https://api.example/b", refused: false},
		"another host":               {from: "https://api.example/a", to: "https://evil.example/a", refused: true},
		"https to http on same host": {from: "https://api.example/a", to: "http://api.example/a", refused: true},
		"http to https on same host": {from: "http://127.0.0.1:9/a", to: "https://127.0.0.1:9/a", refused: false},
	} {
		t.Run(name, func(t *testing.T) {
			err := RefuseOffHostRedirect(redirectHop(t, tc.to), []*http.Request{redirectHop(t, tc.from)})
			if tc.refused && !errors.Is(err, ErrRedirectRefused) {
				t.Errorf("err = %v, want ErrRedirectRefused", err)
			}
			if !tc.refused && err != nil {
				t.Errorf("err = %v, want the redirect followed", err)
			}
		})
	}
}

// Setting CheckRedirect replaces Go's own 10-hop limit, so the guard has to
// keep it, or a same-host redirect loop runs until the attempt times out.
func TestRefuseOffHostRedirectKeepsTheHopLimit(t *testing.T) {
	t.Parallel()

	var via []*http.Request
	for i := 0; i < 10; i++ {
		via = append(via, redirectHop(t, "https://api.example/loop"))
	}
	if err := RefuseOffHostRedirect(redirectHop(t, "https://api.example/loop"), via); err == nil {
		t.Error("an 11th same-host redirect was followed")
	}
	if err := RefuseOffHostRedirect(redirectHop(t, "https://api.example/loop"), via[:9]); err != nil {
		t.Errorf("the 10th redirect was refused: %v", err)
	}
}
