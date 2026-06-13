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
  -- Namespace join ensures we match only the constraint in the public schema,
  -- consistent with the pg_class guards above.
  IF EXISTS (
    SELECT 1 FROM pg_constraint c
    JOIN pg_namespace n ON n.oid = c.connamespace
    WHERE c.conname = 'error_events_api_key_id_fkey'
      AND n.nspname = 'public'
      AND c.conrelid = 'public.error_events'::regclass
      AND c.confdeltype = 'a'
  ) THEN RETURN; END IF;

  -- Remove orphaned rows before (re-)adding the FK so ADD CONSTRAINT validation
  -- cannot fail on stale error_events referencing a deleted api_keys row.
  -- Explicit alias 'e' makes the correlated reference unambiguous.
  DELETE FROM public.error_events AS e
  WHERE e.api_key_id IS NOT NULL
    AND NOT EXISTS (SELECT 1 FROM public.api_keys WHERE id = e.api_key_id);

  ALTER TABLE public.error_events
    DROP CONSTRAINT IF EXISTS error_events_api_key_id_fkey;

  ALTER TABLE public.error_events
    ADD CONSTRAINT error_events_api_key_id_fkey
      FOREIGN KEY (api_key_id) REFERENCES public.api_keys(id) ON DELETE NO ACTION;
END $$;

COMMIT;
