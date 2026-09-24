package sink

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogpublisher/pipeline"
	"github.com/beckn/catalog-core/pkg/catalog"
)

func TestBuildPushBody_CarriesEntryMetadata(t *testing.T) {
	meta := PushMeta{
		ParticipantID: "p1", BppURI: "https://p1.example", MessageID: "m1", TransactionID: "t1",
		Timestamp: "2026-01-01T00:00:00Z", UpdateMode: UpdateModeFull, CatalogType: "REGULAR",
		VisibleTo: []string{"beckn.one/testnet"}, SchemaContext: []string{"https://schema.example/retail"},
	}
	body, err := BuildPushBody(meta, []byte(`{"id":"p/c","resources":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	ctx := got["context"].(map[string]any)
	if ctx["bppId"] != "p1" || ctx["action"] != "catalog/publish" {
		t.Fatalf("context = %+v, want bppId=p1 action=catalog/publish", ctx)
	}
	directives := got["message"].(map[string]any)["publishDirectives"].([]any)
	directive := directives[0].(map[string]any)
	// /publish reads schema types from the directive; context.schemaContext
	// is kept as it was for anything still reading it there.
	if types, _ := directive["schemaTypes"].([]any); len(types) != 1 || types[0] != "https://schema.example/retail" {
		t.Fatalf("directive schemaTypes = %v, want the entry's schema types", directive["schemaTypes"])
	}
	if directive["catalogId"] != "p/c" || directive["catalogType"] != "REGULAR" || directive["updateMode"] != UpdateModeFull {
		t.Fatalf("directive = %+v, want catalogId=p/c catalogType=REGULAR updateMode=FULL", directive)
	}
}

func TestBatchCatalog_FitsInOneBatch(t *testing.T) {
	doc := []byte(`{"id":"p/c","resources":[{"id":"r1"}]}`)
	batches, err := BatchCatalog(doc, 0, UpdateModeFull)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 1 || batches[0].UpdateMode != UpdateModeFull {
		t.Fatalf("batches = %+v, want one FULL batch", batches)
	}
}

func TestBatchCatalog_SplitsOversizedCatalog(t *testing.T) {
	resources := make([]map[string]string, 20)
	for i := range resources {
		resources[i] = map[string]string{"id": strings.Repeat("r", 50)}
	}
	doc, err := json.Marshal(map[string]any{"id": "p/c", "resources": resources, "offers": []any{map[string]string{"id": "o1"}}})
	if err != nil {
		t.Fatal(err)
	}
	batches, err := BatchCatalog(doc, 300, UpdateModeFull)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) < 2 {
		t.Fatalf("batches = %d, want more than one for an oversized catalog", len(batches))
	}
	if batches[0].UpdateMode != UpdateModeFull {
		t.Fatalf("lead batch mode = %q, want FULL", batches[0].UpdateMode)
	}
	for _, b := range batches[1:] {
		if b.UpdateMode != UpdateModeMerge {
			t.Fatalf("spillover batch mode = %q, want MERGE", b.UpdateMode)
		}
		if strings.Contains(string(b.Doc), `"offers"`) {
			t.Fatalf("spillover batch %s carries offers, want lead-only", b.Doc)
		}
	}
	totalResources := 0
	for _, b := range batches {
		r, _ := DocCounts(b.Doc)
		totalResources += r
	}
	if totalResources != len(resources) {
		t.Fatalf("totalResources across batches = %d, want %d (none dropped)", totalResources, len(resources))
	}
}

func TestRollup_AllAckedIsAccepted(t *testing.T) {
	accepted, reason := Rollup([]BatchOutcome{{Acked: true}, {Acked: true}})
	if !accepted || reason != "" {
		t.Fatalf("accepted=%v reason=%q, want true/empty", accepted, reason)
	}
}

func TestRollup_AnyFailureIsRejected(t *testing.T) {
	accepted, reason := Rollup([]BatchOutcome{{Acked: true}, {Acked: false, Reason: "schema invalid"}})
	if accepted || !strings.Contains(reason, "schema invalid") {
		t.Fatalf("accepted=%v reason=%q, want false and the failure reason", accepted, reason)
	}
}

func TestDiscoverySink_Send_PushesAndReportsAccepted(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewDiscoverySink(srv.URL, "p1", "https://p1.example", 0, 5*time.Second)
	entry := catalog.CatalogEntry{CatalogID: "p/c", CatalogType: "REGULAR", NetworkIDs: []string{"beckn.one/testnet"}}
	outcome, err := s.Send(context.Background(), entry, []byte(`{"id":"p/c","resources":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Accepted {
		t.Fatalf("outcome = %+v, want accepted", outcome)
	}
	directive := gotBody["message"].(map[string]any)["publishDirectives"].([]any)[0].(map[string]any)
	if directive["catalogType"] != "REGULAR" {
		t.Fatalf("directive = %+v, want catalogType REGULAR from the entry", directive)
	}
	// /publish rejects FULL as unsupported, so a crawled catalogue goes MERGE.
	if directive["updateMode"] != UpdateModeMerge {
		t.Fatalf("directive = %+v, want updateMode MERGE", directive)
	}
	if gotBody["context"].(map[string]any)["action"] != "catalog/publish" {
		t.Fatalf("context = %+v, want action catalog/publish", gotBody["context"])
	}
}

func TestDiscoverySink_Send_RejectionIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("schema validation failed"))
	}))
	defer srv.Close()

	s := NewDiscoverySink(srv.URL, "p1", "https://p1.example", 0, 5*time.Second)
	outcome, err := s.Send(context.Background(), catalog.CatalogEntry{CatalogID: "p/c"}, []byte(`{"id":"p/c","resources":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Accepted || !strings.Contains(outcome.Reason, "schema validation failed") {
		t.Fatalf("outcome = %+v, want rejected with the response body as reason", outcome)
	}
}

// onPublish answers 200 with one on_publish result carrying status.
func onPublish(t *testing.T, status string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{
			"results": []any{map[string]any{"catalogId": "p/c", "status": status, "reason": "resources missing"}}}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// /publish answers 200 even when it did not accept the catalogue: the verdict
// is in message.results. Only ACCEPTED is an ack -- a PARTIAL indexed with
// resources missing, and must not read as success.
func TestPush_JudgesTheOnPublishVerdict(t *testing.T) {
	for status, wantAcked := range map[string]bool{"ACCEPTED": true, "accepted": true, "PARTIAL": false, "REJECTED": false} {
		t.Run(status, func(t *testing.T) {
			out, err := NewClient(5*time.Second).Push(context.Background(), onPublish(t, status).URL, []byte(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			if out.Acked != wantAcked {
				t.Fatalf("Acked = %v for %s, want %v", out.Acked, status, wantAcked)
			}
			if !wantAcked && (!strings.Contains(out.Reason, status) || !strings.Contains(out.Reason, "resources missing")) {
				t.Fatalf("Reason = %q, want the status and the adapter's reason", out.Reason)
			}
		})
	}
}

// A 200 with no results keeps the original rule: 200 is an ack.
func TestPush_200WithoutResultsIsStillAnAck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	out, err := NewClient(5*time.Second).Push(context.Background(), srv.URL, []byte(`{}`))
	if err != nil || !out.Acked {
		t.Fatalf("out = %+v err = %v, want acked", out, err)
	}
}

// Publish is what a pipeline calls: its body goes VERBATIM to <base>/publish
// through the same Client.Push, and the outcome maps onto the pipeline's.
func TestDiscoverySink_Publish_PostsVerbatimAndMapsTheOutcome(t *testing.T) {
	body := []byte(`{"context":{"action":"catalog/publish"},"message":{"catalogs":[{"id":"p/c"}]}}`)
	var got []byte
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		got, _ = io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{
			"results": []any{map[string]any{"catalogId": "p/c", "status": "ACCEPTED"}}}})
	}))
	defer srv.Close()

	s := NewDiscoverySink("", "", "", 0, 5*time.Second)
	outcome := s.Publish(context.Background(), srv.URL+"/", body) // trailing slash tolerated
	if outcome.Status != pipeline.StatusPublished {
		t.Fatalf("outcome = %+v, want published", outcome)
	}
	if path != "/publish" || string(got) != string(body) {
		t.Fatalf("posted %s to %q, want the body verbatim to /publish", got, path)
	}
}

func TestDiscoverySink_Publish_MapsFailures(t *testing.T) {
	s := NewDiscoverySink("", "", "", 0, 5*time.Second)

	if got := s.Publish(context.Background(), onPublish(t, "PARTIAL").URL, []byte(`{}`)); got.Status != pipeline.StatusRejected {
		t.Errorf("PARTIAL: outcome = %+v, want rejected", got)
	}

	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "signature validation failed", http.StatusUnauthorized)
	}))
	defer denied.Close()
	got := s.Publish(context.Background(), denied.URL, []byte(`{}`))
	if got.Status != pipeline.StatusTransportError || !strings.Contains(got.Reason, "401") ||
		!strings.Contains(got.Reason, "signature validation failed") {
		t.Errorf("HTTP 401: outcome = %+v, want a transport error naming the status and body", got)
	}

	if got := s.Publish(context.Background(), "http://127.0.0.1:1", []byte(`{}`)); got.Status != pipeline.StatusTransportError {
		t.Errorf("unreachable: outcome = %+v, want a transport error", got)
	}
}

// A 200 whose body cannot be read as an on_publish answer is NOT an ack: a
// PARTIAL cut off mid-body, or an HTML page from a misrouted proxy, would
// otherwise be reported as published.
func TestPush_UnreadableAnswerIsNotAnAck(t *testing.T) {
	for name, body := range map[string]string{
		"truncated": `{"message":{"results":[{"catalogId":"p/c","status":"PAR`,
		"html":      `<html><body>OK</body></html>`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			out, err := NewClient(5*time.Second).Push(context.Background(), srv.URL, []byte(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			if out.Acked || !strings.Contains(out.Reason, "unreadable") {
				t.Fatalf("out = %+v, want not acked with an unreadable-answer reason", out)
			}
		})
	}
}

// An on_publish answer larger than the old 64 KiB cap is still read whole, so
// a PARTIAL listing many per-resource errors is judged, not truncated.
func TestPush_ReadsALargeAnswerWhole(t *testing.T) {
	errs := make([]map[string]string, 3000)
	for i := range errs {
		errs[i] = map[string]string{"code": "GEOMETRY_CAP", "message": "resource over the 256-geometry cap"}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{
			"results": []any{map[string]any{"catalogId": "p/c", "status": "PARTIAL", "errors": errs}}}})
	}))
	defer srv.Close()
	out, err := NewClient(5*time.Second).Push(context.Background(), srv.URL, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if out.Acked || !strings.Contains(out.Reason, "PARTIAL") || !strings.Contains(out.Reason, "256-geometry cap") {
		t.Fatalf("out = %+v, want a PARTIAL rejection carrying the first error", out)
	}
}

// Publish maps a 200 that carries no results to published -- the same "no
// results keeps the 200 rule" Push applies -- so the two paths agree.
func TestDiscoverySink_Publish_200WithoutResultsIsPublished(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":{}}`))
	}))
	defer srv.Close()
	got := NewDiscoverySink("", "", "", 0, 5*time.Second).Publish(context.Background(), srv.URL, []byte(`{}`))
	if got.Status != pipeline.StatusPublished {
		t.Fatalf("outcome = %+v, want published", got)
	}
}

// A split catalogue goes as several requests, every one of them MERGE:
// /publish rejects FULL, so not even the lead batch may carry it.
func TestDiscoverySink_Send_EveryBatchIsMerge(t *testing.T) {
	resources := make([]map[string]string, 20)
	for i := range resources {
		resources[i] = map[string]string{"id": strings.Repeat("r", 50)}
	}
	doc, _ := json.Marshal(map[string]any{"id": "p/c", "resources": resources})

	var modes []any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		modes = append(modes, body["message"].(map[string]any)["publishDirectives"].([]any)[0].(map[string]any)["updateMode"])
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewDiscoverySink(srv.URL, "p1", "https://p1.example", 300, 5*time.Second)
	if _, err := s.Send(context.Background(), catalog.CatalogEntry{CatalogID: "p/c", CatalogType: "REGULAR"}, doc); err != nil {
		t.Fatal(err)
	}
	if len(modes) < 2 {
		t.Fatalf("sent %d requests, want the catalogue split", len(modes))
	}
	for i, mode := range modes {
		if mode != UpdateModeMerge {
			t.Errorf("request %d updateMode = %v, want MERGE", i, mode)
		}
	}
}
