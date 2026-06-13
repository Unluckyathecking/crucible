BEGIN;

-- Change error_events.api_key_id FK from the default NO ACTION to ON DELETE SET NULL.
-- Rationale: the test harness deletes api_keys before error_events during cleanup.
-- Without this change, an async errorlog.Record goroutine that fires between the
-- DELETE FROM error_events and DELETE FROM api_keys cleanup steps can insert a new
-- error_events row referencing the api_key, causing the subsequent DELETE FROM
-- api_keys to fail with a FK constraint violation and leaving orphaned rows.
-- With ON DELETE SET NULL, deleting api_keys nullifies the reference rather than
-- blocking the delete. Any goroutine that fires after api_keys are deleted gets a
-- FK violation on the now-absent api_key_id and logs "error event record failed" —
-- the existing graceful-failure path in errorlog.Record — so no row is orphaned.
--
-- Idempotent: the DO block checks delete_rule before altering; safe to re-run.
DO $$
BEGIN
  -- Use pg_constraint rather than information_schema: the information_schema
  -- delete_rule column is character_data (blank-padded per SQL standard), so
  -- comparing against 'SET NULL' may fail due to trailing spaces.
  -- pg_constraint.confdeltype is a single char with no padding: 'n' = SET NULL.
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'error_events_api_key_id_fkey'
      AND confdeltype = 'n'
  ) THEN
    ALTER TABLE error_events DROP CONSTRAINT IF EXISTS error_events_api_key_id_fkey;
    ALTER TABLE error_events ADD CONSTRAINT error_events_api_key_id_fkey
      FOREIGN KEY (api_key_id) REFERENCES api_keys(id) ON DELETE SET NULL;
  END IF;
END $$;

COMMIT;
