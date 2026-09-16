package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/beckn-one/beckn-onix/tools/publish/internal/catalogpublish"
)

func TestPublishWiringMatchesMandiFilePrefix(t *testing.T) {
	dir := t.TempDir()
	body := `{"context":{"action":"catalog/publish"},"message":{"catalogs":[{"id":"agmarknet-mock/mandi-MH"}],` +
		`"publishDirectives":[{"catalogId":"agmarknet-mock/mandi-MH"}]}}`
	if err := os.WriteFile(filepath.Join(dir, "mandi-MH.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write catalog file: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"results":[{"catalogId":"agmarknet-mock/mandi-MH","status":"ACCEPTED"}]}}`))
	}))
	defer server.Close()

	result, err := catalogpublish.Publish(context.Background(), catalogpublish.Config{
		PublishURL:     server.URL,
		CatalogIn:      dir,
		FilenamePrefix: catalogFilePrefix,
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(result.Outcomes) != 1 || result.Outcomes[0].StateCode != "MH" {
		t.Fatalf("outcomes = %+v, want mandi-MH.json matched and posted", result.Outcomes)
	}
}

func TestPublishWiringRetiresTheStatedOldCatalogID(t *testing.T) {
	var posted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &posted)
		_, _ = w.Write([]byte(`{"message":{"results":[{"catalogId":"` + oldCatalogID + `","status":"ACCEPTED"}]}}`))
	}))
	defer server.Close()

	result, err := catalogpublish.Publish(context.Background(), catalogpublish.Config{
		PublishURL:     server.URL,
		CatalogIn:      t.TempDir(), // empty; RetireOld alone must still post
		RetireOld:      true,
		OldCatalogID:   oldCatalogID,
		FilenamePrefix: catalogFilePrefix,
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if result.RetiredOld == nil || result.RetiredOld.CatalogID != oldCatalogID {
		t.Fatalf("RetiredOld = %+v, want %q", result.RetiredOld, oldCatalogID)
	}
	catalog := posted["message"].(map[string]any)["catalogs"].([]any)[0].(map[string]any)
	if catalog["id"] != oldCatalogID {
		t.Errorf("posted tombstone id = %v, want %q", catalog["id"], oldCatalogID)
	}
}
