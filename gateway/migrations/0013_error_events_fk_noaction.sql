BEGIN;

-- Restore error_events.api_key_id FK to the original NO ACTION default.
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
-- Idempotent two-phase approach:
--   Phase 1: drop the constraint if it exists with the wrong delete action.
--   Phase 2: add the constraint if it does not exist (handles fresh DBs and phase 1 drop).
-- Schema-qualified via pg_namespace join AND schema-prefixed regclass casts to avoid
-- search_path ambiguity.
--
-- PostgreSQL confdeltype codes: 'a'=NO ACTION, 'n'=SET NULL, 'c'=CASCADE,
-- 'r'=RESTRICT, 'd'=SET DEFAULT. Phase 1 drops anything that is not 'a' (NO ACTION).
DO $$
BEGIN
  -- Phase 1: drop if present with non-NO ACTION delete semantics (e.g., SET NULL='n').
  IF EXISTS (
    SELECT 1 FROM pg_constraint c
    JOIN pg_namespace n ON n.oid = c.connamespace
    WHERE n.nspname   = 'public'
      AND c.conname   = 'error_events_api_key_id_fkey'
      AND c.conrelid  = 'public.error_events'::regclass
      AND c.confrelid = 'public.api_keys'::regclass
      AND c.contype   = 'f'
      AND c.confdeltype <> 'a'
  ) THEN
    ALTER TABLE error_events DROP CONSTRAINT error_events_api_key_id_fkey;
  END IF;

  -- Phase 2: add if absent (fresh DB or just dropped above).
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint c
    JOIN pg_namespace n ON n.oid = c.connamespace
    WHERE n.nspname   = 'public'
      AND c.conname   = 'error_events_api_key_id_fkey'
      AND c.conrelid  = 'public.error_events'::regclass
      AND c.confrelid = 'public.api_keys'::regclass
      AND c.contype   = 'f'
  ) THEN
    ALTER TABLE error_events ADD CONSTRAINT error_events_api_key_id_fkey
      FOREIGN KEY (api_key_id) REFERENCES api_keys(id);
  END IF;
END $$;

COMMIT;
