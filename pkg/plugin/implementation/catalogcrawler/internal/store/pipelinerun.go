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

// ClaimPipelineRun takes pipeline's firing before the run does any work,
// reporting false when the firing is already served or another replica holds
// it.
//
// It is the run-log write moved to the START of the run and made
// conditional: last_run_at is set to now only while it is still before
// firing, the instant the current schedule window opened. Two replicas racing
// are ordered by the row lock, and the loser's WHERE sees the winner's now and
// matches nothing -- so RETURNING yields zero rows and it stands down.
//
// A replica that crashes mid-run leaves its claim in place, and that firing
// is then treated as served until the next one. That is the price of this
// shape; ReleasePipelineRun covers the ordinary failure.
func (s *Store) ClaimPipelineRun(ctx context.Context, pipeline string, now, firing time.Time) (bool, error) {
	var claimed string
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO crawler_pipeline_run (pipeline, last_run_at)
		 VALUES ($1, $2)
		 ON CONFLICT (pipeline) DO UPDATE
		    SET last_run_at = EXCLUDED.last_run_at
		  WHERE crawler_pipeline_run.last_run_at < $3
		 RETURNING pipeline`,
		pipeline, now, firing).Scan(&claimed)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: ClaimPipelineRun: %w", err)
	}
	return true, nil
}

// ReleasePipelineRun gives back a claim whose run failed, restoring the
// marker to previous -- what LastPipelineRun read before the claim -- so the
// next tick, on this replica or another, retries the same firing. A zero
// previous means the pipeline had never run, and the row is removed.
func (s *Store) ReleasePipelineRun(ctx context.Context, pipeline string, previous time.Time) error {
	var err error
	if previous.IsZero() {
		_, err = s.db.ExecContext(ctx, `DELETE FROM crawler_pipeline_run WHERE pipeline=$1`, pipeline)
	} else {
		_, err = s.db.ExecContext(ctx,
			`UPDATE crawler_pipeline_run SET last_run_at=$2 WHERE pipeline=$1`, pipeline, previous)
	}
	if err != nil {
		return fmt.Errorf("store: ReleasePipelineRun: %w", err)
	}
	return nil
}
