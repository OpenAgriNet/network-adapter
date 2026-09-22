package pipeline

// files_test.go covers writing a run's catalogues and clearing the previous
// run's. The stale sweep is the part worth pinning: it must remove exactly the
// set the publish step would otherwise send, and nothing else in a directory
// an operator chose.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteCataloguesWritesOneFilePerSlug(t *testing.T) {
	built := []Catalogue{
		{Slug: "MH", CatalogID: "catalog:example:MH", Content: []byte(`{"context":{"a":1}}`)},
		{Slug: "MH-2", CatalogID: "catalog:example:MH-2", Content: []byte(`{"context":{"a":2}}`)},
		{Slug: "KA", CatalogID: "catalog:example:KA", Content: []byte(`{"context":{"a":3}}`)},
	}

	// A directory that does not exist yet: WriteCatalogues is what a fresh run
	// relies on to create its output directory.
	dir := filepath.Join(t.TempDir(), "catalog")
	if err := WriteCatalogues(built, dir, "example"); err != nil {
		t.Fatalf("WriteCatalogues: %v", err)
	}

	for _, name := range []string{"example-KA.json", "example-MH.json", "example-MH-2.json"} {
		content, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var doc map[string]any
		if err := json.Unmarshal(content, &doc); err != nil {
			t.Errorf("%s is not valid JSON: %v", name, err)
		}
		// Pretty-printed for the human who reviews these files before a
		// publish, which is the only reason they hit disk at all.
		if !bytes.Contains(content, []byte("\n  \"context\"")) {
			t.Errorf("%s is not indented: %s", name, content)
		}
	}

	// And nothing else: a stale file from an earlier slug would be published
	// as though it were current.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 3 {
		t.Errorf("wrote %d files, want 3", len(entries))
	}
}

// TestWriteCataloguesRemovesStaleCataloguesFromAnEarlierRun covers a bug that
// is invisible on the first run and wrong from the second.
//
// WriteCatalogues writes only the slugs built THIS run, but the publish step
// globs every <prefix>-*.json in the directory and posts all of them. So if a
// state splits into MH and MH-2 on Monday and fits in one catalogue on
// Tuesday, Tuesday's run republishes Monday's stale example-MH-2.json as
// though it were current. A human running this by hand could see the
// directory; the daily loop this package is built for cannot.
func TestWriteCataloguesRemovesStaleCataloguesFromAnEarlierRun(t *testing.T) {
	dir := t.TempDir()

	// Monday: a split state left two catalogues behind.
	stale := filepath.Join(dir, "example-MH-2.json")
	if err := os.WriteFile(stale, []byte(`{"stale":true}`), 0o644); err != nil {
		t.Fatalf("seed stale catalogue: %v", err)
	}
	// Something that is not ours, in the same directory. It must survive:
	// this directory is an operator's to point wherever they like.
	bystander := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(bystander, []byte("keep me"), 0o644); err != nil {
		t.Fatalf("seed bystander: %v", err)
	}
	// Another pipeline's catalogue, under its own prefix. Also must survive --
	// this is the sweep half of the two-pipelines-one-directory corruption.
	neighbour := filepath.Join(dir, "weather-KA.json")
	if err := os.WriteFile(neighbour, []byte(`{"someone else":true}`), 0o644); err != nil {
		t.Fatalf("seed neighbour: %v", err)
	}

	// Tuesday: the state fits in one catalogue.
	built := []Catalogue{{
		Slug:      "MH",
		CatalogID: "catalog:example:MH",
		Content:   []byte(`{"current":true}`),
	}}
	if err := WriteCatalogues(built, dir, "example"); err != nil {
		t.Fatalf("WriteCatalogues: %v", err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("yesterday's catalogue survived; the publish step would post it again as today's")
	}
	if _, err := os.Stat(bystander); err != nil {
		t.Errorf("a file that was not ours was deleted: %v", err)
	}
	if _, err := os.Stat(neighbour); err != nil {
		t.Errorf("another pipeline's catalogue was deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "example-MH.json")); err != nil {
		t.Errorf("today's catalogue was not written: %v", err)
	}
}
