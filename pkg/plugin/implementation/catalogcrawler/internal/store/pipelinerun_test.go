package store

// pipelinerun_test.go — exercises LastPipelineRun/RecordPipelineRun against a
// REAL Postgres, because what is being tested is the SQL (the upsert's
// ON CONFLICT, and timestamptz surviving a round trip): a mock would only
// replay whatever these statements were assumed to do.
//
// Skipped unless CATALOGCRAWLER_TEST_DSN names a database the test may
// migrate into, so `go test ./...` stays hermetic -- the same opt-in gating
// the other live tests in this repo use.
//
// Tests share that one database and never truncate it; each keys its rows on
// a pipeline name derived from t.Name(), so they cannot collide with each
// other or with leftovers from an earlier run.

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"
)

// storeOrSkip returns a migrated Store for CATALOGCRAWLER_TEST_DSN, skipping
// the test when no database is configured.
func storeOrSkip(t *testing.T) (*Store, *sql.DB) {
	t.Helper()

	dsn := os.Getenv("CATALOGCRAWLER_TEST_DSN")
	if dsn == "" {
		t.Skip("db test: set CATALOGCRAWLER_TEST_DSN to a scratch Postgres database to run")
	}
	db, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping %s: %v", dsn, err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return New(db), db
}

// pipelineName keys a test's rows on its own name, so tests sharing the
// database stay independent without truncating it.
func pipelineName(t *testing.T) string {
	t.Helper()
	return "test/" + t.Name()
}

// A pipeline that has never run is a normal state for a fresh deployment:
// zero time, and no error for the caller to have to special-case.
func TestStore_LastPipelineRun_NeverRan(t *testing.T) {
	s, db := storeOrSkip(t)
	ctx := context.Background()
	name := pipelineName(t)

	// Guard against a leftover row from an earlier run of this same test.
	if _, err := db.ExecContext(ctx, `DELETE FROM crawler_pipeline_run WHERE pipeline=$1`, name); err != nil {
		t.Fatalf("clearing row: %v", err)
	}

	at, err := s.LastPipelineRun(ctx, name)
	if err != nil {
		t.Fatalf("LastPipelineRun: unexpected error for a never-run pipeline: %v", err)
	}
	if !at.IsZero() {
		t.Fatalf("LastPipelineRun = %v, want the zero time for a never-run pipeline", at)
	}
}

// The instant must survive the round trip, including its offset: the schedule
// this feeds is resolved in a named timezone, so a run recorded at 06:30 IST
// must not read back as 06:30 in the server's zone.
func TestStore_RecordPipelineRun_RoundTrip(t *testing.T) {
	s, _ := storeOrSkip(t)
	ctx := context.Background()
	name := pipelineName(t)

	ist := time.FixedZone("IST", 5*60*60+30*60)
	want := time.Date(2026, 3, 1, 6, 30, 0, 0, ist)

	if err := s.RecordPipelineRun(ctx, name, want); err != nil {
		t.Fatalf("RecordPipelineRun: %v", err)
	}
	got, err := s.LastPipelineRun(ctx, name)
	if err != nil {
		t.Fatalf("LastPipelineRun: %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("LastPipelineRun = %v, want the same instant as %v", got, want)
	}
}

// Recording again for the same pipeline is the common case (it happens on
// every tick), so it must overwrite rather than fail on the primary key or
// leave two rows behind for LastPipelineRun to pick between.
func TestStore_RecordPipelineRun_OverwritesSamePipeline(t *testing.T) {
	s, db := storeOrSkip(t)
	ctx := context.Background()
	name := pipelineName(t)

	first := time.Date(2026, 3, 1, 6, 30, 0, 0, time.UTC)
	second := first.Add(24 * time.Hour)

	if err := s.RecordPipelineRun(ctx, name, first); err != nil {
		t.Fatalf("RecordPipelineRun (first): %v", err)
	}
	if err := s.RecordPipelineRun(ctx, name, second); err != nil {
		t.Fatalf("RecordPipelineRun (second): %v", err)
	}

	got, err := s.LastPipelineRun(ctx, name)
	if err != nil {
		t.Fatalf("LastPipelineRun: %v", err)
	}
	if !got.Equal(second) {
		t.Fatalf("LastPipelineRun = %v, want the second recorded run %v", got, second)
	}

	var rows int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM crawler_pipeline_run WHERE pipeline=$1`, name).Scan(&rows); err != nil {
		t.Fatalf("counting rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("crawler_pipeline_run has %d rows for %q, want 1", rows, name)
	}
}

// A pipeline name is caller-supplied data, never SQL: a name carrying quotes
// is stored and read back verbatim rather than terminating the statement.
func TestStore_PipelineRun_NameIsParameterised(t *testing.T) {
	s, _ := storeOrSkip(t)
	ctx := context.Background()
	name := pipelineName(t) + `'; DROP TABLE crawler_pipeline_run; --`

	want := time.Date(2026, 3, 2, 6, 30, 0, 0, time.UTC)
	if err := s.RecordPipelineRun(ctx, name, want); err != nil {
		t.Fatalf("RecordPipelineRun: %v", err)
	}
	got, err := s.LastPipelineRun(ctx, name)
	if err != nil {
		t.Fatalf("LastPipelineRun: %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("LastPipelineRun = %v, want %v", got, want)
	}
}
