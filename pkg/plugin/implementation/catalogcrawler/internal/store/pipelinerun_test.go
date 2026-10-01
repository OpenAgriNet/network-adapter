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
	"sync"
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

// freshPipeline is pipelineName with any row an earlier run of the same test
// left behind removed first. The claim tests depend on starting from
// never-run; the database is shared and never truncated.
func freshPipeline(t *testing.T, db *sql.DB) string {
	t.Helper()
	key := pipelineName(t)
	if _, err := db.ExecContext(context.Background(),
		`DELETE FROM crawler_pipeline_run WHERE pipeline=$1`, key); err != nil {
		t.Fatalf("clearing %s: %v", key, err)
	}
	return key
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

// Two replicas racing for one firing: the conditional upsert lets exactly one
// through. The loser gets false and must not run.
func TestStore_ClaimPipelineRun_OneWinnerPerFiring(t *testing.T) {
	s, db := storeOrSkip(t)
	ctx := context.Background()
	key := freshPipeline(t, db)
	firing := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	now := firing.Add(5 * time.Minute)

	first, err := s.ClaimPipelineRun(ctx, key, now, firing)
	if err != nil || !first {
		t.Fatalf("first claim = %v, %v; want true", first, err)
	}
	second, err := s.ClaimPipelineRun(ctx, key, now.Add(time.Second), firing)
	if err != nil || second {
		t.Fatalf("second claim = %v, %v; want false, the firing is taken", second, err)
	}
	if got, _ := s.LastPipelineRun(ctx, key); !got.Equal(now) {
		t.Fatalf("last run = %v, want the winning claim's %v", got, now)
	}
}

// A firing an earlier run already served cannot be claimed; the next firing can.
func TestStore_ClaimPipelineRun_ServedFiringIsNotClaimedAgain(t *testing.T) {
	s, db := storeOrSkip(t)
	ctx := context.Background()
	key := freshPipeline(t, db)
	firing := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)

	if err := s.RecordPipelineRun(ctx, key, firing.Add(time.Minute)); err != nil {
		t.Fatalf("RecordPipelineRun: %v", err)
	}
	if ok, err := s.ClaimPipelineRun(ctx, key, firing.Add(time.Hour), firing); err != nil || ok {
		t.Fatalf("claim of a served firing = %v, %v; want false", ok, err)
	}
	next := firing.Add(24 * time.Hour)
	if ok, err := s.ClaimPipelineRun(ctx, key, next.Add(time.Minute), next); err != nil || !ok {
		t.Fatalf("claim of the next firing = %v, %v; want true", ok, err)
	}
}

// A failed run gives its claim back: last_run_at returns to what it was, so
// the next tick -- on any replica -- retries the same firing.
func TestStore_ReleasePipelineRun_RestoresThePreviousRun(t *testing.T) {
	s, db := storeOrSkip(t)
	ctx := context.Background()
	key := freshPipeline(t, db)
	previous := time.Date(2026, 9, 20, 0, 1, 0, 0, time.UTC)
	firing := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)

	if err := s.RecordPipelineRun(ctx, key, previous); err != nil {
		t.Fatalf("RecordPipelineRun: %v", err)
	}
	if ok, _ := s.ClaimPipelineRun(ctx, key, firing.Add(time.Minute), firing); !ok {
		t.Fatal("claim failed")
	}
	if err := s.ReleasePipelineRun(ctx, key, previous); err != nil {
		t.Fatalf("ReleasePipelineRun: %v", err)
	}
	if got, _ := s.LastPipelineRun(ctx, key); !got.Equal(previous) {
		t.Fatalf("last run = %v, want the restored %v", got, previous)
	}
	if ok, _ := s.ClaimPipelineRun(ctx, key, firing.Add(2*time.Minute), firing); !ok {
		t.Fatal("a released firing could not be claimed again")
	}
}

// Releasing a claim on a pipeline that had never run removes the row, so it
// reads as never-run again rather than as having run at the claim instant.
func TestStore_ReleasePipelineRun_OfAFirstEverClaimForgetsIt(t *testing.T) {
	s, db := storeOrSkip(t)
	ctx := context.Background()
	key := freshPipeline(t, db)
	firing := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)

	if ok, _ := s.ClaimPipelineRun(ctx, key, firing.Add(time.Minute), firing); !ok {
		t.Fatal("claim failed")
	}
	if err := s.ReleasePipelineRun(ctx, key, time.Time{}); err != nil {
		t.Fatalf("ReleasePipelineRun: %v", err)
	}
	if got, _ := s.LastPipelineRun(ctx, key); !got.IsZero() {
		t.Fatalf("last run = %v, want never-run", got)
	}
}

// Ten replicas claiming the same firing at the same moment: exactly one wins.
// This is the double-publish the claim exists to stop, exercised against the
// real row lock rather than a sequence of calls.
func TestStore_ClaimPipelineRun_ConcurrentClaimantsOneWins(t *testing.T) {
	s, db := storeOrSkip(t)
	ctx := context.Background()
	key := freshPipeline(t, db)
	firing := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)

	const replicas = 10
	var wg sync.WaitGroup
	wins := make(chan bool, replicas)
	for i := 0; i < replicas; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := s.ClaimPipelineRun(ctx, key, firing.Add(time.Duration(i+1)*time.Second), firing)
			if err != nil {
				t.Errorf("replica %d: %v", i, err)
			}
			wins <- ok
		}(i)
	}
	wg.Wait()
	close(wins)
	won := 0
	for ok := range wins {
		if ok {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("%d replicas claimed the firing, want exactly 1", won)
	}
}
