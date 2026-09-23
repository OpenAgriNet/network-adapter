package pipeline

// run_execute_test.go covers the half of a run that run_test.go stops short
// of: everything after the schedule says "go".
//
// It runs the fixture pipeline in testdata/ against a fake upstream, through
// the real interpreter and the real catalogue builder. Nothing here is stubbed
// except the upstream itself, so what is under test is the path production
// takes.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

// firedAt is an instant the fixture's midnight schedule has already fired for.
func firedAt(t *testing.T) time.Time {
	t.Helper()
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	return time.Date(2026, 9, 21, 6, 0, 0, 0, ist)
}

// The whole path: token, step, mapping, grouping, chunking, render, write.
//
// The fixture has four things in two groups and a chunk budget of 2, so group
// AA (three things) must split into two catalogues and BB must not.
func TestRunExecutesTheFileAndWritesWhatItDescribes(t *testing.T) {
	upstream := newFixtureUpstream(t, twoGroupsOfThings)
	outDir := t.TempDir()
	runLog := &fakeRunLog{}

	report, err := Run(context.Background(), RunOptions{
		Pipeline: fixturePipeline(),
		Record:   publishingRecord(),
		RunLog:   runLog,
		Lookup:   fixtureEnv(upstream.URL),
		Now:      firedAt(t),
		OutDir:   outDir,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// AA splits at the budget, BB does not: three catalogues in all.
	if len(report.Catalogues) != 3 {
		t.Fatalf("got %d catalogues, want 3 (AA, AA-2, BB): %+v", len(report.Catalogues), report.Catalogues)
	}

	bySlug := map[string]Catalogue{}
	for _, catalogue := range report.Catalogues {
		bySlug[catalogue.Slug] = catalogue
	}
	for _, slug := range []string{"AA", "AA-2", "BB"} {
		catalogue, ok := bySlug[slug]
		if !ok {
			t.Fatalf("no catalogue for %s; got %v", slug, report.Catalogues)
		}
		if want := "catalog:example:" + slug; catalogue.CatalogID != want {
			t.Errorf("%s catalogId = %q, want %q -- the identity template did not resolve",
				slug, catalogue.CatalogID, want)
		}
		// The file's `output.filenamePrefix` is not used for the filename --
		// metadata.name is -- so assert what actually reaches disk.
		path := filepath.Join(report.OutDir, "example-"+slug+".json")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("catalogue for %s was not written: %v", slug, err)
		}
	}

	// The chunk boundary, from the rendered document rather than the report:
	// the first chunk carries the budget and the second the remainder.
	if got := decodeFixtureCatalogue(t, bySlug["AA"].Content); got.Count != 2 {
		t.Errorf("AA carries %v records, want 2 (the chunk budget)", got.Count)
	}
	if got := decodeFixtureCatalogue(t, bySlug["AA-2"].Content); got.Count != 1 {
		t.Errorf("AA-2 carries %v records, want 1 (the remainder)", got.Count)
	}

	if report.Published != nil {
		t.Error("a run with Publish unset reported a publish result")
	}
	if len(runLog.recorded) != 1 {
		t.Errorf("recorded %d runs, want 1", len(runLog.recorded))
	}
}

// The corruption this design exists to prevent: two DIFFERENT pipelines
// pointed at one configured directory must not delete or publish each other's
// catalogues.
//
// The stale sweep runs before every collection and deletes by filename prefix,
// and the publish step globs by the same prefix -- so if both pipelines wrote
// into the shared directory, the second run would erase the first's and then
// post whatever was left as its own. It would look healthy on both sides.
func TestRunKeepsEachPipelineInItsOwnDirectory(t *testing.T) {
	upstream := newFixtureUpstream(t, twoGroupsOfThings)
	shared := t.TempDir()
	now := firedAt(t)

	run := func(files Files) RunReport {
		t.Helper()
		record := publishingRecord()
		record.Actions["publish"] = model.ActionPlan{Mappings: files.RegistryPath}

		report, err := Run(context.Background(), RunOptions{
			Pipeline: files,
			Record:   record,
			Lookup:   fixtureEnv(upstream.URL),
			Now:      now,
			OutDir:   shared,
		})
		if err != nil {
			t.Fatalf("Run(%s): %v", files.Path, err)
		}
		return report
	}

	first := run(fixturePipeline())
	second := run(otherPipeline())

	if first.OutDir == second.OutDir {
		t.Fatalf("both pipelines wrote to %s; each would sweep away the other's catalogues", first.OutDir)
	}
	if filepath.Dir(first.OutDir) != shared || filepath.Dir(second.OutDir) != shared {
		t.Errorf("directories %q and %q are not both beneath %q", first.OutDir, second.OutDir, shared)
	}

	// The first pipeline's catalogues must have survived the second's run --
	// this is what fails if the sweep is pointed at a shared directory.
	if _, err := os.Stat(filepath.Join(first.OutDir, "example-AA.json")); err != nil {
		t.Errorf("the first pipeline's catalogue did not survive the second's run: %v", err)
	}
}

// A step that fails must fail the run, and the run must not be recorded: a
// transient upstream outage at midnight is retried on the next tick rather
// than costing the whole day.
func TestRunDoesNotRecordAFailedRun(t *testing.T) {
	runLog := &fakeRunLog{}

	_, err := Run(context.Background(), RunOptions{
		Pipeline: fixturePipeline(),
		Record:   publishingRecord(),
		RunLog:   runLog,
		Lookup:   fixtureEnv("http://127.0.0.1:1"), // nothing listens
		Now:      firedAt(t),
		OutDir:   t.TempDir(),
	})
	if err == nil {
		t.Fatal("a run against an unreachable upstream reported success")
	}
	if len(runLog.recorded) != 0 {
		t.Error("a failed run was recorded; the day's retry is now lost")
	}
}

// failWhenEmpty is the file's own refusal: an empty result walks cleanly
// through every later step and produces a well-formed, zero-error, empty run.
func TestRunHonoursTheFilesFailWhenEmpty(t *testing.T) {
	upstream := newFixtureUpstream(t, `[]`)

	_, err := Run(context.Background(), RunOptions{
		Pipeline: fixturePipeline(),
		Record:   publishingRecord(),
		Lookup:   fixtureEnv(upstream.URL),
		Now:      firedAt(t),
		OutDir:   t.TempDir(),
	})
	if err == nil {
		t.Fatal("an empty upstream produced a successful, empty run")
	}
}

func TestRunRefusesWithoutAPipeline(t *testing.T) {
	if _, err := Run(context.Background(), RunOptions{Record: publishingRecord()}); err == nil {
		t.Fatal("a run with no pipeline was attempted")
	}
}

// The registry may sanction a path that is not this pipeline's. Running the
// embedded one anyway would publish under a capability nobody authorised.
func TestRunRefusesAPipelineTheRegistryDidNotName(t *testing.T) {
	upstream := newFixtureUpstream(t, twoGroupsOfThings)

	record := publishingRecord()
	record.Actions["publish"] = model.ActionPlan{
		Mappings: "pkg/plugin/implementation/SomethingElse/its-own-pipeline.yaml",
	}

	_, err := Run(context.Background(), RunOptions{
		Pipeline: fixturePipeline(),
		Record:   record,
		Lookup:   fixtureEnv(upstream.URL),
		Now:      firedAt(t),
	})
	if err == nil {
		t.Fatal("a pipeline the registry did not name was run")
	}
	assertNeverCalled(t, upstream)
}
