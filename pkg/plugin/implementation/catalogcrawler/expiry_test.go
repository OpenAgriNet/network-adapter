package catalogcrawler

import (
	"encoding/json"
	"testing"
	"time"
)

func validityOf(t *testing.T, document json.RawMessage) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(document, &doc); err != nil {
		t.Fatalf("stamped document is not JSON: %v", err)
	}
	validity, _ := doc["validity"].(map[string]any)
	return validity
}

func TestACatalogWithNoValidityGetsOurExpiry(t *testing.T) {
	until := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)

	stamped, err := withExpiry(json.RawMessage(`{"id":"n2:c"}`), until)
	if err != nil {
		t.Fatalf("withExpiry: %v", err)
	}
	if got := validityOf(t, stamped)["endDate"]; got != "2026-10-16T09:00:00Z" {
		t.Errorf("endDate = %v, want our expiry", got)
	}
}

func TestStampingKeepsEverythingElseInTheDocument(t *testing.T) {
	until := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	document := json.RawMessage(`{"id":"n2:c","descriptor":{"name":"onion"},"bppUri":"https://n2/provider/"}`)

	stamped, err := withExpiry(document, until)
	if err != nil {
		t.Fatalf("withExpiry: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(stamped, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["bppUri"] != "https://n2/provider/" {
		t.Errorf("bppUri = %v, want the owner's untouched -- a select follows it back", doc["bppUri"])
	}
	if d, _ := doc["descriptor"].(map[string]any); d["name"] != "onion" {
		t.Errorf("descriptor = %v, want it untouched", doc["descriptor"])
	}
}

func TestStampingLeavesTheOwnersOtherValidityAlone(t *testing.T) {
	until := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	document := json.RawMessage(`{"id":"n2:c","validity":{"startDate":"2026-10-01T00:00:00Z","startTime":"09:00","endTime":"17:00"}}`)

	stamped, err := withExpiry(document, until)
	if err != nil {
		t.Fatalf("withExpiry: %v", err)
	}
	v := validityOf(t, stamped)
	if v["startDate"] != "2026-10-01T00:00:00Z" {
		t.Errorf("startDate = %v, want the owner's -- we never bring a catalog live early", v["startDate"])
	}
	if v["startTime"] != "09:00" || v["endTime"] != "17:00" {
		t.Errorf("daily window = %v/%v, want the owner's opening hours untouched", v["startTime"], v["endTime"])
	}
}

func TestOurExpiryReplacesTheOwnersEndDate(t *testing.T) {
	until := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	document := json.RawMessage(`{"id":"n2:c","validity":{"endDate":"2026-12-31T00:00:00Z"}}`)

	stamped, err := withExpiry(document, until)
	if err != nil {
		t.Fatalf("withExpiry: %v", err)
	}
	if got := validityOf(t, stamped)["endDate"]; got != "2026-10-16T09:00:00Z" {
		t.Errorf("endDate = %v, want ours -- we hold a copy only as long as we keep refreshing it", got)
	}
}

func TestADocumentThatIsNotAnObjectIsRefused(t *testing.T) {
	if _, err := withExpiry(json.RawMessage(`["not","a","catalog"]`), time.Now()); err == nil {
		t.Error("a document that is not an object was stamped instead of being refused")
	}
}
