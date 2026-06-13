BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

-- Restore error_events.api_key_id FK to ON DELETE NO ACTION.
-- pg_constraint.confdeltype codes (https://www.postgresql.org/docs/current/catalog-pg-constraint.html):
--   'a'=NO ACTION (target), 'r'=RESTRICT, 'c'=CASCADE, 'n'=SET NULL, 'd'=SET DEFAULT
DO $$
DECLARE
  actual_conname text;
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'public' AND c.relname = 'error_events' AND c.relkind = 'r'
  ) THEN RETURN; END IF;

  IF NOT EXISTS (
    SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'public' AND c.relname = 'api_keys' AND c.relkind = 'r'
  ) THEN RETURN; END IF;

  -- Find the FK from error_events -> api_keys with wrong delete rule.
  -- Capturing the actual constraint name avoids assumptions about naming conventions.
  -- All three namespace joins (constraint, table, referenced table) are qualified so
  -- the query cannot match a same-named constraint in a different schema.
  SELECT c.conname INTO actual_conname
  FROM   pg_constraint c
  JOIN   pg_namespace  n   ON n.oid   = c.connamespace
  JOIN   pg_class      t   ON t.oid   = c.conrelid
  JOIN   pg_namespace  nt  ON nt.oid  = t.relnamespace
  JOIN   pg_class      rf  ON rf.oid  = c.confrelid
  JOIN   pg_namespace  nrf ON nrf.oid = rf.relnamespace
  WHERE  c.contype     = 'f'
    AND  c.confdeltype != 'a'   -- not NO ACTION: needs fixing
    AND  n.nspname     = 'public'
    AND  t.relname     = 'error_events'
    AND  nt.nspname    = 'public'
    AND  rf.relname    = 'api_keys'
    AND  nrf.nspname   = 'public'
  LIMIT 1;

  IF FOUND THEN
    -- Lock both tables: ADD CONSTRAINT requires ACCESS EXCLUSIVE on the referenced
    -- table (api_keys) to validate the FK, so locking only error_events would allow
    -- concurrent DML on api_keys to block or deadlock the migration.
    LOCK TABLE public.error_events, public.api_keys IN ACCESS EXCLUSIVE MODE;
    RAISE NOTICE 'migration 0013: fixing error_events FK % to ON DELETE NO ACTION', actual_conname;
    -- Drop by captured name so the migration succeeds even if the constraint was
    -- previously recreated under a non-canonical name.
    EXECUTE format('ALTER TABLE public.error_events DROP CONSTRAINT %I', actual_conname);
    ALTER TABLE public.error_events
      ADD CONSTRAINT error_events_api_key_id_fkey
        FOREIGN KEY (api_key_id) REFERENCES public.api_keys(id) ON DELETE NO ACTION;
  ELSE
    RAISE NOTICE 'migration 0013: error_events FK already NO ACTION or absent, skipping';
  END IF;
END $$;

COMMIT;
