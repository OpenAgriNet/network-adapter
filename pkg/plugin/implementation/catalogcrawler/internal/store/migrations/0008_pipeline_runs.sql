-- Last-run marker per pipeline (Store.LastPipelineRun /
-- Store.RecordPipelineRun): a scheduled pipeline reads this on startup to
-- decide whether its window has already been served, so a process restart
-- does not re-fire a daily run that already happened. One row per pipeline,
-- overwritten in place -- this is a cursor, not a run history.
--
-- timestamptz, not timestamp: the schedule this gates is resolved in a named
-- timezone, and a naive timestamp would be written as a wall-clock reading
-- and silently reinterpreted in whatever zone the server session happens to
-- be in, moving the recorded run by that offset.
--
-- Idempotent (IF NOT EXISTS) so Migrate can run on every startup.

CREATE TABLE IF NOT EXISTS crawler_pipeline_run (
  pipeline    text PRIMARY KEY,
  last_run_at timestamptz NOT NULL
);
