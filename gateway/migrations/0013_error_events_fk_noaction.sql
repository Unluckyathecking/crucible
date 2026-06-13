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
--
-- PostgreSQL confdeltype codes: 'a'=NO ACTION, 'n'=SET NULL, 'c'=CASCADE,
-- 'r'=RESTRICT, 'd'=SET DEFAULT. Phase 1 drops anything that is not 'a' (NO ACTION).
--
-- Table resolution uses pg_class + pg_namespace joins on relname/nspname strings,
-- avoiding regclass casts and to_regclass() so there is no oid/regclass type mismatch
-- and no NULL comparison risk when a table does not yet exist.
DO $$
BEGIN
  -- Phase 1: drop if present with non-NO ACTION delete semantics (e.g., SET NULL='n').
  IF EXISTS (
    SELECT 1 FROM pg_constraint c
    JOIN pg_class     et ON et.oid = c.conrelid
    JOIN pg_class     ak ON ak.oid = c.confrelid
    JOIN pg_namespace en ON en.oid = et.relnamespace
    JOIN pg_namespace an ON an.oid = ak.relnamespace
    WHERE en.nspname = 'public' AND et.relname = 'error_events'
      AND an.nspname = 'public' AND ak.relname = 'api_keys'
      AND c.conname  = 'error_events_api_key_id_fkey'
      AND c.contype  = 'f'
      AND c.confdeltype <> 'a'
  ) THEN
    ALTER TABLE public.error_events DROP CONSTRAINT error_events_api_key_id_fkey;
  END IF;

  -- Phase 2: add if absent (fresh DB or just dropped by Phase 1 above).
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint c
    JOIN pg_class     et ON et.oid = c.conrelid
    JOIN pg_class     ak ON ak.oid = c.confrelid
    JOIN pg_namespace en ON en.oid = et.relnamespace
    JOIN pg_namespace an ON an.oid = ak.relnamespace
    WHERE en.nspname = 'public' AND et.relname = 'error_events'
      AND an.nspname = 'public' AND ak.relname = 'api_keys'
      AND c.conname  = 'error_events_api_key_id_fkey'
      AND c.contype  = 'f'
  ) THEN
    ALTER TABLE public.error_events ADD CONSTRAINT error_events_api_key_id_fkey
      FOREIGN KEY (api_key_id) REFERENCES public.api_keys(id);
  END IF;
END $$;

COMMIT;
