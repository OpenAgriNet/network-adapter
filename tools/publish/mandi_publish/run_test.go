package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// run() is the one-stage path: collect from the upstream and write catalogs.
// There is no collection document -- the catalogs are the only output.

func TestRunWritesCatalogsWithoutACollectionFile(t *testing.T) {
	upstream := fakeUpstream(t, "")
	defer upstream.Close()

	dir := t.TempDir()
	catalogOut := filepath.Join(dir, "catalog")

	built, summary, collection, err := run(context.Background(), runConfig{
		collect: config{
			baseURL:  upstream.URL,
			user:     "user",
			secret:   "secret",
			fromDate: "01-07-2026",
			toDate:   "01-12-2026",
		},
		build: buildConfig{
			catalogOut:    catalogOut,
			participantID: "agmarknet-mock",
			networkID:     "oan-dev",
		},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(built) == 0 {
		t.Fatalf("built nothing; summary %+v", summary)
	}
	if len(collection.Markets) == 0 {
		t.Error("the collection is returned so a summary can report on it, and it is empty")
	}

	// The catalogs are on disk, ready to post.
	for _, state := range built {
		if _, err := os.Stat(state.Path); err != nil {
			t.Errorf("catalog for %s not written: %v", state.StateCode, err)
		}
	}

	// And nothing wrote a collection document, because none was asked for.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			t.Errorf("run wrote %s; only the catalog directory was asked for", entry.Name())
		}
	}
}

func TestRunRefusesMissingCredentialsBeforeTouchingDisk(t *testing.T) {
	dir := t.TempDir()
	catalogOut := filepath.Join(dir, "catalog")

	_, _, _, err := run(context.Background(), runConfig{
		collect: config{baseURL: "http://example.invalid", fromDate: "01-07-2026", toDate: "01-12-2026"},
		build:   buildConfig{catalogOut: catalogOut},
	})
	if err == nil {
		t.Fatal("run accepted empty credentials")
	}
	if _, statErr := os.Stat(catalogOut); statErr == nil {
		t.Error("run created the catalog directory before it had credentials")
	}
}
