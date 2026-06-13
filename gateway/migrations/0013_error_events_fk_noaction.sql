BEGIN;

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

  IF NOT EXISTS (
    SELECT 1
    FROM   pg_constraint c
    JOIN   pg_class      t ON t.oid = c.conrelid
    JOIN   pg_namespace  n ON n.oid = t.relnamespace
    WHERE  c.conname     = 'error_events_api_key_id_fkey'
      AND  c.contype     = 'f'
      AND  c.confdeltype = 'a'
      AND  t.relname     = 'error_events'
      AND  n.nspname     = 'public'
  ) THEN
    LOCK TABLE public.error_events IN ACCESS EXCLUSIVE MODE;
    RAISE NOTICE 'migration 0013: fixing error_events FK from SET NULL to NO ACTION';
    -- ADD NOT VALID first so there is no window with zero FK coverage.
    ALTER TABLE public.error_events
      DROP CONSTRAINT IF EXISTS error_events_api_key_id_fkey_new;
    ALTER TABLE public.error_events
      ADD CONSTRAINT error_events_api_key_id_fkey_new
        FOREIGN KEY (api_key_id) REFERENCES public.api_keys(id) ON DELETE NO ACTION NOT VALID;
    -- VALIDATE acquires ShareUpdateExclusiveLock (lighter than AccessExclusive).
    ALTER TABLE public.error_events
      VALIDATE CONSTRAINT error_events_api_key_id_fkey_new;
    ALTER TABLE public.error_events
      DROP CONSTRAINT IF EXISTS error_events_api_key_id_fkey;
    ALTER TABLE public.error_events
      RENAME CONSTRAINT error_events_api_key_id_fkey_new TO error_events_api_key_id_fkey;
  ELSE
    RAISE NOTICE 'migration 0013: error_events FK already NO ACTION, skipping';
  END IF;
END $$;

COMMIT;
