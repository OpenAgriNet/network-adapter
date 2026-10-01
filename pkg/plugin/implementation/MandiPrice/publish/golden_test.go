package publish

// golden_test.go pins the EXACT bytes this pipeline produces.
//
// Every other test here asserts a property -- two catalogs, this many
// resources, that field present. A golden file asserts the whole document, so
// a change nobody described shows up as a diff rather than as a test that
// still passes because it never looked at the field that moved.
//
// It exists to make the risky changes safe to check: renaming the package and
// the file, deleting fields the engine does not read, rewriting the mappings.
// Any of those can alter the output silently. After this, none can.
//
// Regenerate deliberately, never reflexively:
//
//	go test ./pkg/plugin/implementation/MandiPrice/publish/ -run Golden -update
//
// and READ the diff. A golden file updated without reading it is worse than
// no golden file, because it reads as review.

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogpublisher/pipeline"
)

var update = flag.Bool("update", false, "rewrite the golden file from this run's output")

const goldenPath = "testdata/golden/catalogs.json"

// The whole pipeline, end to end, against the fake upstream, compared byte
// for byte.
func TestGoldenCatalogs(t *testing.T) {
	upstream := fakeAgmarknet(t, twoGoodStates())

	report, err := pipeline.Run(context.Background(), pipeline.RunOptions{
		Pipeline: Pipeline(),
		Record:   publishingRecord(),
		Lookup:   fakeUpstreamEnv(upstream.URL),
		Now:      firingTime(t),
		OutDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Catalogs) == 0 {
		t.Fatal("the run produced no catalogs, so the golden file would pin nothing")
	}

	got := renderGolden(t, report)

	if *update {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("creating the golden directory: %v", err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("writing %s: %v", goldenPath, err)
		}
		t.Logf("golden file rewritten: %s -- read the diff before committing it", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("reading %s (run with -update to create it): %v", goldenPath, err)
	}
	if string(got) != string(want) {
		// The whole documents, not a first-difference offset: the reader
		// needs to see WHAT changed, and these are small.
		t.Errorf("the pipeline's output no longer matches %s.\n\n--- want ---\n%s\n\n--- got ---\n%s",
			goldenPath, want, got)
	}
}

// renderGolden turns a report into the stable document the golden file holds.
//
// Order is imposed rather than assumed: grouping walks a map, so the slugs
// come back in whatever order Go felt like. A golden file that re-sorted
// itself every run would fail for a reason that is not a regression.
func renderGolden(t *testing.T, report pipeline.RunReport) []byte {
	t.Helper()
	return renderGoldenWith(t, report, volatile)
}

// renderGoldenWith is renderGolden with its own set of volatile fields.
func renderGoldenWith(t *testing.T, report pipeline.RunReport, keys map[string]string) []byte {
	t.Helper()

	type entry struct {
		Slug      string          `json:"slug"`
		CatalogID string          `json:"catalogId"`
		Content   json.RawMessage `json:"content"`
	}

	entries := make([]entry, 0, len(report.Catalogs))
	for _, catalog := range report.Catalogs {
		// Re-encoded through a generic decode so the golden file is
		// canonically formatted and key order is stable, rather than carrying
		// whatever spacing the renderer emitted.
		var document any
		if err := json.Unmarshal(catalog.Content, &document); err != nil {
			t.Fatalf("catalog %s is not JSON: %v", catalog.Slug, err)
		}
		stabiliseKeys(document, keys)
		canonical, err := json.Marshal(document)
		if err != nil {
			t.Fatalf("re-encoding catalog %s: %v", catalog.Slug, err)
		}
		entries = append(entries, entry{
			Slug:      catalog.Slug,
			CatalogID: catalog.CatalogID,
			Content:   canonical,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Slug < entries[j].Slug })

	out, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		t.Fatalf("encoding the golden document: %v", err)
	}
	return append(out, '\n')
}

// volatile names the fields that differ on every run for reasons that are not
// regressions: two freshly-minted UUIDs and the wall clock.
//
// They are REPLACED, not deleted, so the golden file still fails if one stops
// being emitted -- which would be a real change to the envelope, and exactly
// the kind this file exists to catch.
var volatile = map[string]string{
	"transactionId": "<uuid>",
	"messageId":     "<uuid>",
	"timestamp":     "<timestamp>",
}

// stabilise walks a decoded document and substitutes the volatile fields in
// place, at any depth: the envelope has them at the top and a resource could
// grow its own.
func stabilise(node any) { stabiliseKeys(node, volatile) }

// stabiliseKeys is stabilise over a given set of volatile fields.
func stabiliseKeys(node any, keys map[string]string) {
	switch typed := node.(type) {
	case map[string]any:
		for key, value := range typed {
			if placeholder, isVolatile := keys[key]; isVolatile {
				if _, isString := value.(string); isString {
					typed[key] = placeholder
					continue
				}
			}
			stabiliseKeys(value, keys)
		}
	case []any:
		for _, item := range typed {
			stabiliseKeys(item, keys)
		}
	}
}

const goldenDirectPath = "testdata/golden/catalogs-direct.json"

// directVolatile adds the Direct resources' wall-clock fields: a price is
// published with the moment it was generated and is valid for a day from it.
var directVolatile = map[string]string{
	"transactionId": "<uuid>",
	"messageId":     "<uuid>",
	"timestamp":     "<timestamp>",
	"generatedAt":   "<timestamp>",
	"startsAt":      "<timestamp>",
	"endsAt":        "<timestamp + 2 days>",
	"startDate":     "<timestamp>",
	"endDate":       "<timestamp + 2 days>",
}

// The Direct half, end to end against the fake upstream, byte for byte.
// Regenerate with -update, and read the diff.
func TestGoldenDirectCatalogs(t *testing.T) {
	upstream := fakeAgmarknet(t, twoGoodStates())

	report, err := pipeline.Run(context.Background(), pipeline.RunOptions{
		Pipeline: Pipeline(),
		Record:   publishingRecord(),
		Lookup:   modeEnv(upstream.URL, "direct"),
		Now:      firingTime(t),
		OutDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Catalogs) == 0 {
		t.Fatal("the run produced no catalogs, so the golden file would pin nothing")
	}

	got := renderGoldenWith(t, report, directVolatile)

	if *update {
		if err := os.WriteFile(goldenDirectPath, got, 0o644); err != nil {
			t.Fatalf("writing %s: %v", goldenDirectPath, err)
		}
		t.Logf("golden file rewritten: %s -- read the diff before committing it", goldenDirectPath)
		return
	}

	want, err := os.ReadFile(goldenDirectPath)
	if err != nil {
		t.Fatalf("reading %s (run with -update to create it): %v", goldenDirectPath, err)
	}
	if string(got) != string(want) {
		t.Errorf("the Direct output no longer matches %s.\n\n--- want ---\n%s\n\n--- got ---\n%s",
			goldenDirectPath, want, got)
	}
}
