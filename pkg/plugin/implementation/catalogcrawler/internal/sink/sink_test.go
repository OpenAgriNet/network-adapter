package sink

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogpublisher/pipeline"
	"github.com/beckn-one/beckn-onix/pkg/telemetry"
	"github.com/beckn/catalog-core/pkg/catalog"
	"go.opentelemetry.io/otel/attribute"
)

func TestBuildPushBody_CarriesEntryMetadata(t *testing.T) {
	meta := PushMeta{
		SenderID: "p1", ReceiverID: "discovery.example", MessageID: "m1", TransactionID: "t1",
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
	if ctx["senderId"] != "p1" || ctx["receiverId"] != "discovery.example" || ctx["action"] != "catalog/publish" {
		t.Fatalf("context = %+v, want senderId=p1 receiverId=discovery.example action=catalog/publish", ctx)
	}
	for _, old := range []string{"bppId", "bppUri", "bapId", "bapUri"} {
		if _, present := ctx[old]; present {
			t.Errorf("context carries %q; sender/receiver are the only identity fields", old)
		}
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

	s := NewDiscoverySink(srv.URL, "p1", "discovery.example", 0, 5*time.Second)
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
	// /publish rejects FULL as unsupported, so a crawled catalog goes MERGE.
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

	s := NewDiscoverySink(srv.URL, "p1", "discovery.example", 0, 5*time.Second)
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

// /publish answers 200 even when it did not accept the catalog: the verdict
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

// A split catalog goes as several requests, every one of them MERGE:
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

	s := NewDiscoverySink(srv.URL, "p1", "discovery.example", 300, 5*time.Second)
	if _, err := s.Send(context.Background(), catalog.CatalogEntry{CatalogID: "p/c", CatalogType: "REGULAR"}, doc); err != nil {
		t.Fatal(err)
	}
	if len(modes) < 2 {
		t.Fatalf("sent %d requests, want the catalog split", len(modes))
	}
	for i, mode := range modes {
		if mode != UpdateModeMerge {
			t.Errorf("request %d updateMode = %v, want MERGE", i, mode)
		}
	}
}

// Pipeline-built bodies carry no identity of their own -- the mapping does not
// know which deployment it runs in -- so the sink stamps it on the way out.
func TestPublishStampsTheDeploymentIdentity(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"message":{"results":[{"status":"ACCEPTED"}]}}`))
	}))
	defer server.Close()

	d := NewDiscoverySink("", "sender.example", "discovery.example", 0, 5*time.Second)
	body := []byte(`{"context":{"action":"catalog/publish","version":"2.0.0"},"message":{"catalogs":[{"id":"c1"}]}}`)
	if out := d.Publish(context.Background(), server.URL, body); out.Status != pipeline.StatusPublished {
		t.Fatalf("outcome = %+v", out)
	}
	ctx := got["context"].(map[string]any)
	if ctx["senderId"] != "sender.example" || ctx["receiverId"] != "discovery.example" {
		t.Fatalf("context = %v; want senderId/receiverId stamped", ctx)
	}
	if _, present := ctx["bppId"]; present {
		t.Errorf("context carries bppId: %v", ctx)
	}
	if ctx["action"] != "catalog/publish" || ctx["version"] != "2.0.0" {
		t.Fatalf("stamping lost the existing context: %v", ctx)
	}
	if got["message"].(map[string]any)["catalogs"].([]any)[0].(map[string]any)["id"] != "c1" {
		t.Fatalf("stamping changed the message: %v", got["message"])
	}
}

// A sink with no identity configured changes nothing: the body goes as built.
func TestPublishWithoutIdentityPostsTheBodyVerbatim(t *testing.T) {
	var got []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"message":{"results":[{"status":"ACCEPTED"}]}}`))
	}))
	defer server.Close()

	body := []byte(`{"context":{"action":"catalog/publish"},"message":{}}`)
	NewDiscoverySink("", "", "", 0, 5*time.Second).Publish(context.Background(), server.URL, body)
	if string(got) != string(body) {
		t.Fatalf("posted %s, want the body verbatim %s", got, body)
	}
}

func TestRetireSendsAnInactiveCatalogWithIdentity(t *testing.T) {
	var got map[string]any
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"message":{"results":[{"status":"ACCEPTED"}]}}`))
	}))
	defer server.Close()

	d := NewDiscoverySink("", "sender.example", "discovery.example", 0, 5*time.Second)
	out := d.Retire(context.Background(), server.URL, pipeline.Retirement{
		CatalogID: "cat-old", DescriptorName: "Retired", CatalogType: "REGULAR", UpdateMode: "MERGE",
	})
	if out.Status != pipeline.StatusPublished || out.CatalogID != "cat-old" {
		t.Fatalf("outcome = %+v", out)
	}
	if path != "/publish" {
		t.Errorf("posted to %q, want /publish", path)
	}
	ctx := got["context"].(map[string]any)
	if ctx["senderId"] != "sender.example" || ctx["receiverId"] != "discovery.example" {
		t.Fatalf("retire context = %v", ctx)
	}
	message := got["message"].(map[string]any)
	catalog := message["catalogs"].([]any)[0].(map[string]any)
	if catalog["id"] != "cat-old" || catalog["isActive"] != false || len(catalog["resources"].([]any)) != 0 {
		t.Fatalf("retired catalog = %v; want cat-old, isActive false, no resources", catalog)
	}
	directive := message["publishDirectives"].([]any)[0].(map[string]any)
	if directive["catalogId"] != "cat-old" || directive["updateMode"] != "MERGE" || directive["catalogType"] != "REGULAR" {
		t.Fatalf("directive = %v", directive)
	}
}

// One line per publish call, with the path but never the query.
func TestPushLogsEachCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"results":[{"status":"ACCEPTED"}]}}`))
	}))
	defer server.Close()
	var buf bytes.Buffer
	c := NewClient(5 * time.Second)
	c.Log = slog.New(slog.NewTextHandler(&buf, nil))
	if _, err := c.Push(context.Background(), server.URL+"/publish?sig=abc", []byte(`{}`)); err != nil {
		t.Fatalf("Push: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"publish call", "method=POST", "path=/publish", "status=200", "duration=", "bytes="} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "sig=abc") {
		t.Errorf("the query reached the log:\n%s", out)
	}
}

// TestPushEmitsAnAuditRecord proves a push goes through the SAME audit
// pipeline (telemetry.EmitAuditLogs) every inbound Beckn action goes
// through -- not a second, hand-rolled log line -- and that it carries the
// correlation ids and identity BuildPushBody already put in the body.
func TestPushEmitsAnAuditRecord(t *testing.T) {
	ctx := context.Background()
	provider, exporter, err := telemetry.NewTestProviderWithLogs(ctx)
	if err != nil {
		t.Fatalf("NewTestProviderWithLogs: %v", err)
	}
	defer provider.Shutdown(ctx)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"results":[{"status":"ACCEPTED"}]}}`))
	}))
	defer server.Close()

	body := []byte(`{"context":{"transactionId":"t1","messageId":"m1","senderId":"agmarknet"}}`)
	c := NewClient(5 * time.Second)
	if _, err := c.Push(ctx, server.URL+"/publish", body); err != nil {
		t.Fatalf("Push: %v", err)
	}

	records := exporter.Records()
	if len(records) != 1 {
		t.Fatalf("want 1 audit record, got %d", len(records))
	}

	got := map[string]string{}
	var statusCode int64
	records[0].WalkAttributes(func(kv attribute.KeyValue) bool {
		if kv.Key == "http.response.status_code" {
			statusCode = kv.Value.AsInt64()
			return true
		}
		got[string(kv.Key)] = kv.Value.AsString()
		return true
	})

	for key, want := range map[string]string{
		"audit.direction": "publish",
		"sender.id":       "agmarknet",
		"receiver.id":     server.URL + "/publish",
		"transaction_id":  "t1",
		"message_id":      "m1",
	} {
		if got[key] != want {
			t.Errorf("attribute %q = %q, want %q (all: %+v)", key, got[key], want, got)
		}
	}
	if statusCode != 200 {
		t.Errorf("http.response.status_code = %d, want 200", statusCode)
	}
	if got["checkSum"] == "" {
		t.Error("audit record missing checkSum -- this is the same helper stdHandler uses, it should always set one")
	}
}

// The audit record says WHAT was published -- ids, counts, size and a hash of
// the exact bytes -- not the catalog itself. A day's catalogs are tens of MB,
// and the record exists to prove a publish happened, which the hash does.
// The receiver is named without its query or credentials.
func TestPushAuditCarriesASummaryNotTheCatalog(t *testing.T) {
	ctx := context.Background()
	provider, exporter, err := telemetry.NewTestProviderWithLogs(ctx)
	if err != nil {
		t.Fatalf("NewTestProviderWithLogs: %v", err)
	}
	defer provider.Shutdown(ctx)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"results":[{"status":"ACCEPTED"}]}}`))
	}))
	defer server.Close()

	body := []byte(`{"context":{"transactionId":"t1","messageId":"m1"},"message":{"catalogs":[
		{"id":"catalog:a","resources":[{"id":"r1","descriptor":{"name":"SECRET-MARKET-NAME"}},{"id":"r2"}]}]}}`)
	endpoint := strings.Replace(server.URL, "http://", "http://user:pw@", 1) + "/publish?sig=abc"
	if _, err := NewClient(5*time.Second).Push(ctx, endpoint, body); err != nil {
		t.Fatalf("Push: %v", err)
	}

	records := exporter.Records()
	if len(records) != 1 {
		t.Fatalf("want 1 audit record, got %d", len(records))
	}
	auditBody := records[0].Body().AsString()
	if strings.Contains(auditBody, "SECRET-MARKET-NAME") {
		t.Errorf("the audit record carries the catalog content: %s", auditBody)
	}
	var summary struct {
		CatalogIDs []string `json:"catalogIds"`
		Resources  int      `json:"resources"`
		Bytes      int      `json:"bytes"`
		SHA256     string   `json:"bodySha256"`
	}
	if err := json.Unmarshal([]byte(auditBody), &summary); err != nil {
		t.Fatalf("audit body is not the summary JSON: %v (%s)", err, auditBody)
	}
	sum := sha256.Sum256(body)
	if fmt.Sprint(summary.CatalogIDs) != "[catalog:a]" || summary.Resources != 2 ||
		summary.Bytes != len(body) || summary.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("summary = %+v; want catalog:a, 2 resources, %d bytes and the body's sha256", summary, len(body))
	}

	var receiver string
	records[0].WalkAttributes(func(kv attribute.KeyValue) bool {
		if kv.Key == "receiver.id" {
			receiver = kv.Value.AsString()
		}
		return true
	})
	if want := server.URL + "/publish"; receiver != want {
		t.Errorf("receiver.id = %q, want %q (no query, no credentials)", receiver, want)
	}
}

// A rejection's reason is its first line, not the whole answer: it is logged
// once per catalog, and an adapter's error page can be a megabyte.
func TestPushRejectionReasonIsTheFirstLine(t *testing.T) {
	long := "schema invalid: " + strings.Repeat("x", 5000) + "\nsecond line\nthird line"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, long, http.StatusBadRequest)
	}))
	defer server.Close()

	out, err := NewClient(5*time.Second).Push(context.Background(), server.URL, []byte(`{}`))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if !strings.HasPrefix(out.Reason, "schema invalid: ") || strings.Contains(out.Reason, "second line") || len(out.Reason) > 210 {
		t.Errorf("Reason = %d bytes (%.80q...), want the first line, bounded", len(out.Reason), out.Reason)
	}
}

// A PARTIAL in the second result is still a failure: every catalog in the
// answer is judged, not just the first.
func TestPushJudgesEveryResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"results":[{"status":"ACCEPTED"},{"status":"PARTIAL","reason":"geometry cap"}]}}`))
	}))
	defer server.Close()
	out, err := NewClient(5*time.Second).Push(context.Background(), server.URL, []byte(`{}`))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if out.Acked || !strings.Contains(out.Reason, "PARTIAL") || !strings.Contains(out.Reason, "geometry cap") {
		t.Fatalf("outcome = %+v; want not acked, PARTIAL: geometry cap", out)
	}
}

// The retirement goes through BuildPushBody with every field the pipeline
// resolved -- the one builder, used fully.
func TestRetireCarriesTheWholeRetirement(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"message":{"results":[{"status":"ACCEPTED"}]}}`))
	}))
	defer server.Close()

	d := NewDiscoverySink("", "sender.example", "discovery.example", 0, 5*time.Second)
	out := d.Retire(context.Background(), server.URL, pipeline.Retirement{
		CatalogID: "cat-old", DescriptorName: "Retired", CatalogType: "MASTER", UpdateMode: "FULL",
		VisibleTo: []string{"oan-prod"}, SchemaTypes: []string{"https://schema.example/ctx.jsonld"},
	})
	if out.Status != pipeline.StatusPublished {
		t.Fatalf("outcome = %+v", out)
	}
	directive := got["message"].(map[string]any)["publishDirectives"].([]any)[0].(map[string]any)
	if directive["catalogType"] != "MASTER" || directive["updateMode"] != "FULL" ||
		fmt.Sprint(directive["visibleTo"]) != "[oan-prod]" ||
		fmt.Sprint(directive["schemaTypes"]) != "[https://schema.example/ctx.jsonld]" {
		t.Fatalf("directive = %v; want the retirement's catalogType, updateMode, visibleTo, schemaTypes", directive)
	}
	ctx := got["context"].(map[string]any)
	if fmt.Sprint(ctx["schemaContext"]) != "[https://schema.example/ctx.jsonld]" || ctx["senderId"] != "sender.example" {
		t.Fatalf("context = %v; want schemaContext and identity", ctx)
	}
}
