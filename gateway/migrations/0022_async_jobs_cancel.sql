-- Adds the 'cancelled' terminal state to async_jobs.status: POST
-- /v1/jobs/{id}/cancel (see gateway/internal/jobs/store.go's CancelQueued)
-- transitions a queued row directly into it. Unlike 0020/0021's
-- ADD COLUMN IF NOT EXISTS additions, widening a CHECK constraint requires
-- dropping and re-adding it under the same name Postgres auto-assigned the
-- original inline CHECK in 0019_async_jobs.sql (<table>_<column>_check).
--
-- Guarded so re-running on every boot (invariant #8) is a genuine no-op: an
-- unconditional DROP + ADD re-validates the CHECK against every row under an
-- ACCESS EXCLUSIVE lock on async_jobs on each boot. The DO block only touches
-- the constraint when the widened definition is not already present, so a
-- second boot does no locking and no re-validation. No version-tracking table.

BEGIN;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM pg_constraint
    WHERE conrelid = 'async_jobs'::regclass
      AND conname = 'async_jobs_status_check'
      AND pg_get_constraintdef(oid) LIKE '%cancelled%'
  ) THEN
    ALTER TABLE async_jobs DROP CONSTRAINT IF EXISTS async_jobs_status_check;
    ALTER TABLE async_jobs ADD CONSTRAINT async_jobs_status_check
      CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled'));
  END IF;
END $$;

COMMIT;
