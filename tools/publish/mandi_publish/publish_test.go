package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// catalogDir writes one catalog file per named state, each a payload shaped
// like the real one but small enough to read in a failure message.
func catalogDir(t *testing.T, states ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, state := range states {
		body := `{"context":{"action":"catalog/publish"},"message":{"catalogs":[{"id":"agmarknet-mock/mandi-` +
			state + `","isActive":true,"resources":[{"id":"res:agmarknet:market:1"}]}],` +
			`"publishDirectives":[{"catalogId":"agmarknet-mock/mandi-` + state + `"}]}}`
		path := filepath.Join(dir, "mandi-"+state+".json")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return dir
}

// ackServer answers every POST with one result carrying the given status.
func ackServer(t *testing.T, status string, errorCount int) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.URL.Path != "/publish" {
			t.Errorf("posted to %q, want /publish", r.URL.Path)
		}
		var envelope map[string]any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &envelope)
		catalogID := "unknown"
		if message, ok := envelope["message"].(map[string]any); ok {
			if directives, ok := message["publishDirectives"].([]any); ok && len(directives) > 0 {
				if first, ok := directives[0].(map[string]any); ok {
					if id, ok := first["catalogId"].(string); ok {
						catalogID = id
					}
				}
			}
		}
		result := map[string]any{"catalogId": catalogID, "status": status}
		if errorCount > 0 {
			errs := make([]any, 0, errorCount)
			for i := 0; i < errorCount; i++ {
				errs = append(errs, map[string]any{
					"code":    "POL_GENERIC_ERROR",
					"message": "catalog carries more than 256 geometries; this one was not indexed",
				})
			}
			result["errors"] = errs
		}
		if status == "REJECTED" {
			result["reason"] = "resource 3 invalid"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"context": map[string]any{"action": "catalog/on_publish"},
			"message": map[string]any{"results": []any{result}},
		})
	}))
	return server, &calls
}

func TestPublishPostsEveryCatalogInTheDirectory(t *testing.T) {
	server, calls := ackServer(t, "ACCEPTED", 0)
	defer server.Close()

	result, err := publish(context.Background(), publishConfig{
		publishURL: server.URL,
		catalogIn:  catalogDir(t, "MH", "AP"),
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	if *calls != 2 {
		t.Errorf("posted %d times, want 2", *calls)
	}
	if len(result.Outcomes) != 2 {
		t.Fatalf("outcomes = %+v, want 2", result.Outcomes)
	}
	// Sorted, so a run's log reads the same way twice.
	if result.Outcomes[0].StateCode != "AP" || result.Outcomes[1].StateCode != "MH" {
		t.Errorf("states = %q, %q, want AP then MH",
			result.Outcomes[0].StateCode, result.Outcomes[1].StateCode)
	}
	if result.Outcomes[1].Status != StatusPublished {
		t.Errorf("status = %q, want published", result.Outcomes[1].Status)
	}
	if result.Outcomes[1].CatalogID != "agmarknet-mock/mandi-MH" {
		t.Errorf("catalogId = %q, want the one from the payload", result.Outcomes[1].CatalogID)
	}
	if result.HasFailures() {
		t.Error("HasFailures is true after two ACCEPTED results")
	}
}

func TestPublishHonoursTheStateFilter(t *testing.T) {
	server, calls := ackServer(t, "ACCEPTED", 0)
	defer server.Close()

	result, err := publish(context.Background(), publishConfig{
		publishURL: server.URL,
		catalogIn:  catalogDir(t, "MH", "AP", "CG"),
		states:     []string{"MH"},
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	if *calls != 1 {
		t.Errorf("posted %d times, want 1", *calls)
	}
	if len(result.Outcomes) != 1 || result.Outcomes[0].StateCode != "MH" {
		t.Errorf("outcomes = %+v, want MH alone", result.Outcomes)
	}
}

func TestPublishFiltersChunkedCatalogsByState(t *testing.T) {
	// A split state is written as mandi-TN-1.json, mandi-TN-2.json. --states TN
	// has to match every chunk of TN, or a filtered republish silently posts
	// part of the state.
	server, calls := ackServer(t, "ACCEPTED", 0)
	defer server.Close()

	dir := catalogDir(t, "TN-1", "TN-2", "MH")

	result, err := publish(context.Background(), publishConfig{
		publishURL: server.URL,
		catalogIn:  dir,
		states:     []string{"TN"},
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	if *calls != 2 {
		t.Errorf("posted %d times, want both TN chunks", *calls)
	}
	if len(result.Outcomes) != 2 {
		t.Fatalf("outcomes = %+v, want both TN chunks", result.Outcomes)
	}
	for _, outcome := range result.Outcomes {
		if outcome.StateCode != "TN" {
			t.Errorf("outcome state = %q, want TN for every chunk", outcome.StateCode)
		}
	}
	if result.Outcomes[0].CatalogID == result.Outcomes[1].CatalogID {
		t.Errorf("both outcomes carry %q; the chunks are different catalogs", result.Outcomes[0].CatalogID)
	}
}

func TestPublishDryRunSendsNothing(t *testing.T) {
	server, calls := ackServer(t, "ACCEPTED", 0)
	defer server.Close()

	result, err := publish(context.Background(), publishConfig{
		publishURL: server.URL,
		catalogIn:  catalogDir(t, "MH"),
		dryRun:     true,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	// A dry run that reached the network would have already published.
	if *calls != 0 {
		t.Errorf("posted %d times during a dry run, want 0", *calls)
	}
	if len(result.Outcomes) != 1 || result.Outcomes[0].Status != StatusDryRun {
		t.Errorf("outcomes = %+v, want one dry-run outcome", result.Outcomes)
	}
	if result.HasFailures() {
		t.Error("HasFailures is true for a dry run")
	}
}

func TestPublishReportsRejected(t *testing.T) {
	server, _ := ackServer(t, "REJECTED", 0)
	defer server.Close()

	result, err := publish(context.Background(), publishConfig{
		publishURL: server.URL,
		catalogIn:  catalogDir(t, "MH"),
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	if result.Outcomes[0].Status != StatusRejected {
		t.Fatalf("status = %q, want rejected", result.Outcomes[0].Status)
	}
	if !strings.Contains(result.Outcomes[0].Reason, "resource 3 invalid") {
		t.Errorf("reason = %q, want the server's stated reason", result.Outcomes[0].Reason)
	}
	if !result.HasFailures() {
		t.Error("HasFailures is false after a REJECTED result")
	}
}

func TestPublishTreatsPartialAsAFailureAndSaysHowMany(t *testing.T) {
	// PARTIAL means the catalog was indexed but some of it was dropped -- the
	// 256-geometry cap. Reporting it as success would hide markets that no
	// proximity search can find.
	server, _ := ackServer(t, "PARTIAL", 288)
	defer server.Close()

	result, err := publish(context.Background(), publishConfig{
		publishURL: server.URL,
		catalogIn:  catalogDir(t, "MH"),
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	if result.Outcomes[0].Status != StatusRejected {
		t.Fatalf("status = %q, want a PARTIAL to count as a failure", result.Outcomes[0].Status)
	}
	reason := result.Outcomes[0].Reason
	if !strings.Contains(reason, "PARTIAL") || !strings.Contains(reason, "288") {
		t.Errorf("reason = %q, want it to name PARTIAL and the error count", reason)
	}
	if !strings.Contains(reason, "256 geometries") {
		t.Errorf("reason = %q, want the first upstream message quoted", reason)
	}
	if !result.HasFailures() {
		t.Error("HasFailures is false after a PARTIAL result")
	}
}

func TestPublishRecordsATransportFailureAndKeepsGoing(t *testing.T) {
	// One unreachable adapter must not discard the other states' outcomes.
	result, err := publish(context.Background(), publishConfig{
		publishURL: "http://127.0.0.1:1", // nothing listens here
		catalogIn:  catalogDir(t, "MH", "AP"),
	})
	if err != nil {
		t.Fatalf("publish returned a fatal error, want per-state outcomes: %v", err)
	}

	if len(result.Outcomes) != 2 {
		t.Fatalf("outcomes = %+v, want both states recorded", result.Outcomes)
	}
	for _, outcome := range result.Outcomes {
		if outcome.Status != StatusTransportError {
			t.Errorf("%s status = %q, want a transport error", outcome.StateCode, outcome.Status)
		}
		if outcome.Reason == "" {
			t.Errorf("%s carries no reason", outcome.StateCode)
		}
	}
	if !result.HasFailures() {
		t.Error("HasFailures is false after two transport errors")
	}
}

func TestPublishRejectsANonSuccessHTTPStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"SCH_SCHEMA_VALIDATION_FAILED"}}`))
	}))
	defer server.Close()

	result, err := publish(context.Background(), publishConfig{
		publishURL: server.URL,
		catalogIn:  catalogDir(t, "MH"),
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	outcome := result.Outcomes[0]
	if outcome.Status != StatusTransportError && outcome.Status != StatusRejected {
		t.Fatalf("status = %q, want a failure", outcome.Status)
	}
	// The adapter's own code is the only thing that says why, so it must
	// survive into the reason rather than being flattened to "400".
	if !strings.Contains(outcome.Reason, "SCH_SCHEMA_VALIDATION_FAILED") {
		t.Errorf("reason = %q, want the upstream code", outcome.Reason)
	}
}

func TestPublishRetiresTheOldCatalogWhenAsked(t *testing.T) {
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var envelope map[string]any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &envelope)
		bodies = append(bodies, envelope)
		_, _ = w.Write([]byte(`{"context":{"action":"catalog/on_publish"},
			"message":{"results":[{"catalogId":"cat-agmarknet-mandi-prices","status":"ACCEPTED"}]}}`))
	}))
	defer server.Close()

	result, err := publish(context.Background(), publishConfig{
		publishURL: server.URL,
		catalogIn:  catalogDir(t),
		retireOld:  true,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	if result.RetiredOld == nil {
		t.Fatal("RetiredOld is nil, want the tombstone's outcome")
	}
	if result.RetiredOld.CatalogID != oldCatalogID {
		t.Errorf("retired %q, want %q", result.RetiredOld.CatalogID, oldCatalogID)
	}
	if result.RetiredOld.Status != StatusPublished {
		t.Errorf("status = %q, want published", result.RetiredOld.Status)
	}

	if len(bodies) != 1 {
		t.Fatalf("posted %d bodies, want just the tombstone", len(bodies))
	}
	catalog := bodies[0]["message"].(map[string]any)["catalogs"].([]any)[0].(map[string]any)
	// isActive false is the whole point: MERGE cannot be relied on to remove
	// the India-wide resource, so the catalog containing it is deactivated.
	if catalog["isActive"] != false {
		t.Errorf("isActive = %v, want false", catalog["isActive"])
	}
	if catalog["id"] != oldCatalogID {
		t.Errorf("tombstone id = %v", catalog["id"])
	}
}

func TestPublishNeedsAnAddress(t *testing.T) {
	_, err := publish(context.Background(), publishConfig{catalogIn: catalogDir(t, "MH")})
	if err == nil {
		t.Fatal("publish accepted an empty URL, want an error")
	}
	if !strings.Contains(err.Error(), "MANDI_PUBLISH_URL") {
		t.Errorf("error = %q, want it to name the variable that sets the address", err)
	}
}

func TestPublishReportsAnEmptyCatalogDirectory(t *testing.T) {
	// Silently publishing nothing looks identical to publishing successfully.
	_, err := publish(context.Background(), publishConfig{
		publishURL: "http://example.invalid",
		catalogIn:  catalogDir(t),
	})
	if err == nil {
		t.Fatal("publish accepted an empty directory, want an error")
	}
}
