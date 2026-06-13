BEGIN;

-- Revert error_events.api_key_id FK to the original NO ACTION default.
--
-- An earlier version of this file changed the FK to ON DELETE SET NULL to allow
-- the test harness cleanup to delete api_keys before error_events. That change
-- was reverted: ON DELETE SET NULL on an audit-log FK is architecturally wrong,
-- since it destroys the link between an error event and the key that caused it.
-- The test harness cleanup handles the FK ordering (error_events before api_keys)
-- without a schema change. In the rare case where an async errorlog.Record goroutine
-- fires between the cleanup's DELETE FROM error_events and DELETE FROM api_keys,
-- the api_keys DELETE is skipped and the orphaned rows are re-cleaned on the next run.
--
-- Idempotent: checks pg_constraint.confdeltype before altering ('a' = NO ACTION).
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'error_events_api_key_id_fkey'
      AND conrelid  = 'error_events'::regclass
      AND confrelid = 'api_keys'::regclass
      AND confdeltype = 'a'
  ) THEN
    ALTER TABLE error_events DROP CONSTRAINT IF EXISTS error_events_api_key_id_fkey;
    ALTER TABLE error_events ADD CONSTRAINT error_events_api_key_id_fkey
      FOREIGN KEY (api_key_id) REFERENCES api_keys(id);
  END IF;
END $$;

COMMIT;
