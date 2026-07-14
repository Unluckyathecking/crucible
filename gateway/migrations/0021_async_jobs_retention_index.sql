-- Retention reaper (see gateway/internal/jobs/reaper.go) periodically DELETEs
-- terminal async_jobs rows older than JOB_RETENTION_DAYS. This partial index
-- keeps that DELETE's WHERE status IN ('succeeded', 'failed') AND updated_at
-- < ... scan cheap without altering or repurposing the three existing
-- async_jobs indexes (idx_async_jobs_queued/_customer/_stuck). Keyed on
-- updated_at, not created_at: Store.Complete/Fail/DeadLetter stamp
-- updated_at on the terminal transition itself, so retention measures age
-- since a row became succeeded/failed rather than since it was enqueued —
-- see reaper.go's deleteBatch doc comment. Idempotent: safe to re-run on
-- every gateway boot (invariant #8) — no version-tracking table.

BEGIN;

CREATE INDEX IF NOT EXISTS idx_async_jobs_retention ON async_jobs(updated_at) WHERE status IN ('succeeded', 'failed');

COMMIT;
