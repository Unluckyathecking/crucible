BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

-- Restore error_events.api_key_id FK to ON DELETE NO ACTION.
-- confdeltype: 'a'=NO ACTION (target), 'n'=SET NULL (incorrect prior state)
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

  -- Fix only when the constraint exists and has the wrong delete rule.
  -- IF EXISTS (confdeltype != 'a') is the natural positive form:
  --   true  = constraint present with wrong type (SET NULL, CASCADE, etc.) = fix
  --   false = constraint already NO ACTION, OR constraint absent = skip
  IF EXISTS (
    SELECT 1
    FROM   pg_constraint c
    JOIN   pg_class      t ON t.oid = c.conrelid
    JOIN   pg_namespace  n ON n.oid = t.relnamespace
    WHERE  c.conname     = 'error_events_api_key_id_fkey'
      AND  c.contype     = 'f'
      AND  c.confdeltype != 'a'   -- not NO ACTION: needs fixing
      AND  t.relname     = 'error_events'
      AND  n.nspname     = 'public'
  ) THEN
    LOCK TABLE public.error_events IN ACCESS EXCLUSIVE MODE;
    RAISE NOTICE 'migration 0013: fixing error_events FK from SET NULL to NO ACTION';
    -- ACCESS EXCLUSIVE lock prevents concurrent DML, so a plain DROP + ADD is safe.
    ALTER TABLE public.error_events
      DROP CONSTRAINT IF EXISTS error_events_api_key_id_fkey;
    ALTER TABLE public.error_events
      ADD CONSTRAINT error_events_api_key_id_fkey
        FOREIGN KEY (api_key_id) REFERENCES public.api_keys(id) ON DELETE NO ACTION;
  ELSE
    RAISE NOTICE 'migration 0013: error_events FK already NO ACTION or absent, skipping';
  END IF;
END $$;

COMMIT;
