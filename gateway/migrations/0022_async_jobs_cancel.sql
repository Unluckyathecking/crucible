-- Adds the 'cancelled' terminal state to async_jobs.status: POST
-- /v1/jobs/{id}/cancel (see gateway/internal/jobs/store.go's CancelQueued)
-- transitions a queued row directly into it. Unlike 0020/0021's
-- ADD COLUMN IF NOT EXISTS additions, widening a CHECK constraint requires
-- dropping and re-adding it — DROP CONSTRAINT IF EXISTS then ADD CONSTRAINT
-- with the same name Postgres auto-assigned the original inline CHECK in
-- 0019_async_jobs.sql (<table>_<column>_check), so this is safe to re-run on
-- every gateway boot (invariant #8) whether or not a prior run of this exact
-- file already applied. No version-tracking table.

BEGIN;

ALTER TABLE async_jobs DROP CONSTRAINT IF EXISTS async_jobs_status_check;
ALTER TABLE async_jobs ADD CONSTRAINT async_jobs_status_check
  CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled'));

COMMIT;
