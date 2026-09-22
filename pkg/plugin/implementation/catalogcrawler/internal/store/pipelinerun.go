package store

// pipelinerun.go — the per-pipeline last-run marker. A scheduled pipeline
// keeps no state in memory across a restart, so without this a daily run
// re-fires every time the process comes back up; the scheduler reads the
// recorded instant to tell an already-served window from a due one.

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// LastPipelineRun returns when pipeline last ran, or the zero time.Time if it
// never has. Never having run is a normal state for a fresh deployment rather
// than a failure, so the missing row is reported as the zero time with a nil
// error instead of leaking sql.ErrNoRows to the caller.
func (s *Store) LastPipelineRun(ctx context.Context, pipeline string) (time.Time, error) {
	var at time.Time
	err := s.db.QueryRowContext(ctx,
		`SELECT last_run_at FROM crawler_pipeline_run WHERE pipeline=$1`, pipeline).
		Scan(&at)
	if err == sql.ErrNoRows {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("store: LastPipelineRun: %w", err)
	}
	return at, nil
}

// RecordPipelineRun records at as pipeline's most recent run, overwriting any
// previous marker (this runs on every completed tick, so the row exists after
// the first one).
func (s *Store) RecordPipelineRun(ctx context.Context, pipeline string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO crawler_pipeline_run (pipeline, last_run_at)
		 VALUES ($1,$2)
		 ON CONFLICT (pipeline) DO UPDATE SET
		   last_run_at = EXCLUDED.last_run_at`,
		pipeline, at)
	if err != nil {
		return fmt.Errorf("store: RecordPipelineRun: %w", err)
	}
	return nil
}
