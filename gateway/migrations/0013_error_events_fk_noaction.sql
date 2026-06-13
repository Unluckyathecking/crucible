BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

-- Ensure error_events.api_key_id FK is ON DELETE NO ACTION.
-- The IF EXISTS guard skips DROP/ADD when the constraint already has the correct
-- delete rule (confdeltype 'a' = NO ACTION), eliminating any window where the FK
-- is briefly absent. Only repairs wrong delete rules or missing constraints.
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'public' AND c.relname = 'error_events' AND c.relkind = 'r'
  ) THEN RETURN; END IF;

  IF NOT EXISTS (
    SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'public' AND c.relname = 'api_keys' AND c.relkind = 'r'
  ) THEN RETURN; END IF;

  -- Skip if constraint already correct: confdeltype 'a' = ON DELETE NO ACTION.
  -- Uses explicit pg_class/pg_namespace joins (no ::regclass cast) so the check
  -- works regardless of search_path, consistent with the guards above.
  IF EXISTS (
    SELECT 1 FROM pg_constraint c
    JOIN pg_namespace n ON n.oid = c.connamespace
    JOIN pg_class r     ON r.oid = c.conrelid
    WHERE c.conname   = 'error_events_api_key_id_fkey'
      AND n.nspname   = 'public'
      AND r.relname   = 'error_events'
      AND c.confdeltype = 'a'
  ) THEN RETURN; END IF;

  -- Remove orphaned rows before (re-)adding the FK so ADD CONSTRAINT validation
  -- cannot fail on stale error_events referencing a deleted api_keys row.
  DELETE FROM public.error_events
  WHERE api_key_id IS NOT NULL
    AND NOT EXISTS (SELECT 1 FROM public.api_keys WHERE id = api_key_id);

  -- Drop the constraint only when it already exists (with the wrong delete rule).
  -- The unconditional DROP (no IF EXISTS) inside the IF branch keeps the pattern
  -- explicit: existence is checked once, then the DROP is guaranteed to succeed.
  IF EXISTS (
    SELECT 1 FROM pg_constraint c
    JOIN pg_namespace n ON n.oid = c.connamespace
    JOIN pg_class r     ON r.oid = c.conrelid
    WHERE c.conname = 'error_events_api_key_id_fkey'
      AND n.nspname = 'public'
      AND r.relname = 'error_events'
  ) THEN
    ALTER TABLE public.error_events
      DROP CONSTRAINT error_events_api_key_id_fkey;
  END IF;

  ALTER TABLE public.error_events
    ADD CONSTRAINT error_events_api_key_id_fkey
      FOREIGN KEY (api_key_id) REFERENCES public.api_keys(id) ON DELETE NO ACTION;
END $$;

COMMIT;
