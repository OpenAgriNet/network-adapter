package sink

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogpublisher/pipeline"
	"github.com/beckn/catalog-core/pkg/catalog"
)

// adapter is a fake provider adapter /publish that answers each request with
// the next status in its list and keeps every body it was sent.
type adapter struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
}

func newAdapter(t *testing.T, statuses ...string) *adapter {
	t.Helper()
	a := &adapter{}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/publish" {
			t.Errorf("posted to %q, want /publish", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		a.mu.Lock()
		status := statuses[len(a.bodies)%len(statuses)]
		a.bodies = append(a.bodies, body)
		a.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{
			"results": []any{map[string]any{"catalogId": "p/c", "status": status, "reason": "because"}}}})
	}))
	t.Cleanup(a.Close)
	return a
}

// A crawled catalogue goes to /publish as catalog/publish, MERGE, carrying
// the directive the index entry declares.
func TestSendPublishesTheCatalogue(t *testing.T) {
	a := newAdapter(t, "ACCEPTED")
	s := NewPublishSink(a.URL, 0, 5*time.Second)
	entry := catalog.CatalogEntry{
		CatalogID: "p/c", CatalogType: "REGULAR",
		NetworkIDs: []string{"oan-dev"}, SchemaTypes: []string{"https://schema.example/x.jsonld"},
	}

	outcome, err := s.Send(context.Background(), entry, []byte(`{"id":"p/c","resources":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Accepted {
		t.Fatalf("outcome = %+v, want accepted", outcome)
	}
	if len(a.bodies) != 1 {
		t.Fatalf("sent %d requests, want 1", len(a.bodies))
	}
	ctx := a.bodies[0]["context"].(map[string]any)
	if ctx["action"] != "catalog/publish" {
		t.Errorf("action = %v, want catalog/publish", ctx["action"])
	}
	d := a.bodies[0]["message"].(map[string]any)["publishDirectives"].([]any)[0].(map[string]any)
	if d["catalogType"] != "REGULAR" || d["updateMode"] != "MERGE" || d["catalogId"] != "p/c" {
		t.Errorf("directive = %v, want REGULAR MERGE p/c", d)
	}
	if len(d["visibleTo"].([]any)) != 1 || len(d["schemaTypes"].([]any)) != 1 {
		t.Errorf("directive = %v, want visibleTo and schemaTypes from the entry", d)
	}
}

// Anything but ACCEPTED is a rejection carrying the adapter's reason --
// PARTIAL included, because a partial index is missing resources.
func TestSendReportsARejection(t *testing.T) {
	a := newAdapter(t, "PARTIAL")
	outcome, err := NewPublishSink(a.URL, 0, 5*time.Second).
		Send(context.Background(), catalog.CatalogEntry{CatalogID: "p/c", CatalogType: "REGULAR"}, []byte(`{"id":"p/c"}`))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Accepted || !strings.Contains(outcome.Reason, "PARTIAL") {
		t.Fatalf("outcome = %+v, want rejected naming PARTIAL", outcome)
	}
}

// A catalogue over the size budget goes as several MERGE requests: every
// resource sent exactly once, offers only with the first. The catalogue is
// accepted only if every request was.
func TestSendSplitsAnOversizedCatalogue(t *testing.T) {
	resources := make([]map[string]string, 20)
	for i := range resources {
		resources[i] = map[string]string{"id": strings.Repeat("r", 50)}
	}
	doc, _ := json.Marshal(map[string]any{"id": "p/c", "resources": resources, "offers": []any{map[string]string{"id": "o1"}}})

	a := newAdapter(t, "ACCEPTED", "ACCEPTED", "REJECTED")
	outcome, err := NewPublishSink(a.URL, 300, 5*time.Second).
		Send(context.Background(), catalog.CatalogEntry{CatalogID: "p/c", CatalogType: "REGULAR"}, doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.bodies) < 3 {
		t.Fatalf("sent %d requests, want the catalogue split", len(a.bodies))
	}

	sent := 0
	for i, body := range a.bodies {
		msg := body["message"].(map[string]any)
		cat := msg["catalogs"].([]any)[0].(map[string]any)
		sent += len(cat["resources"].([]any))
		if _, hasOffers := cat["offers"]; hasOffers != (i == 0) {
			t.Errorf("request %d offers present = %v, want offers only in the first", i, hasOffers)
		}
		if mode := msg["publishDirectives"].([]any)[0].(map[string]any)["updateMode"]; mode != "MERGE" {
			t.Errorf("request %d updateMode = %v, want MERGE", i, mode)
		}
	}
	if sent != len(resources) {
		t.Errorf("sent %d resources across requests, want %d", sent, len(resources))
	}
	if outcome.Accepted {
		t.Error("one request was REJECTED, but the catalogue was reported accepted")
	}
}

// Publish is what a pipeline calls: its built file goes to <base>/publish
// VERBATIM, and the answer is judged ACCEPTED-or-not, PARTIAL a rejection.
func TestPublishPostsTheBodyVerbatimAndJudgesTheAnswer(t *testing.T) {
	body := []byte(`{"context":{"action":"catalog/publish"},"message":{"catalogs":[{"id":"p/c"}]}}`)
	for status, want := range map[string]string{
		"ACCEPTED": pipeline.StatusPublished,
		"PARTIAL":  pipeline.StatusRejected,
		"REJECTED": pipeline.StatusRejected,
	} {
		t.Run(status, func(t *testing.T) {
			var got []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/publish" {
					t.Errorf("posted to %q, want /publish", r.URL.Path)
				}
				got, _ = io.ReadAll(r.Body)
				_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{
					"results": []any{map[string]any{"catalogId": "p/c", "status": status}}}})
			}))
			defer server.Close()

			// A trailing slash on the base must not become //publish.
			outcome := NewPublishSink("", 0, 5*time.Second).Publish(context.Background(), server.URL+"/", body)
			if outcome.Status != want || outcome.CatalogID != "p/c" {
				t.Errorf("outcome = %+v, want %s for p/c", outcome, want)
			}
			if string(got) != string(body) {
				t.Errorf("sent %s, want the body verbatim", got)
			}
		})
	}
}

// A non-2xx answer or an unreachable adapter is a transport error carrying
// what the adapter said -- an Outcome, never a returned error.
func TestPublishReportsTransportFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "signature validation failed", http.StatusUnauthorized)
	}))
	defer server.Close()

	s := NewPublishSink("", 0, 5*time.Second)
	outcome := s.Publish(context.Background(), server.URL, []byte(`{}`))
	if outcome.Status != pipeline.StatusTransportError || !strings.Contains(outcome.Reason, "signature validation failed") {
		t.Errorf("outcome = %+v, want a transport error quoting the adapter", outcome)
	}

	outcome = s.Publish(context.Background(), "http://127.0.0.1:1", []byte(`{}`))
	if outcome.Status != pipeline.StatusTransportError {
		t.Errorf("unreachable adapter: outcome = %+v, want a transport error", outcome)
	}
}
