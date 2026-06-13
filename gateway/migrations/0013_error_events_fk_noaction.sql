BEGIN;

-- Restore error_events.api_key_id FK to ON DELETE NO ACTION.
-- A previous migration incorrectly used ON DELETE SET NULL, which destroyed
-- the audit-log link between an error event and the responsible API key.
--
-- Re-run behaviour: the DO block skips DROP+ADD when the correct NO ACTION
-- constraint already exists (catalog check on confdeltype='a'). The migration
-- runner is single-threaded, so concurrent execution is not a production concern;
-- the LOCK TABLE below makes that guarantee structural rather than operational.
DO $$
BEGIN
  -- Guard: error_events table must exist (skip on fresh DBs with no prior migration).
  IF NOT EXISTS (
    SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'public' AND c.relname = 'error_events' AND c.relkind = 'r'
  ) THEN
    RETURN;
  END IF;

  -- Guard: api_keys table must exist (referenced by the FK we are adding).
  IF NOT EXISTS (
    SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'public' AND c.relname = 'api_keys' AND c.relkind = 'r'
  ) THEN
    RETURN;
  END IF;

  -- Acquire ACCESS EXCLUSIVE on error_events to eliminate the brief window
  -- between DROP CONSTRAINT and ADD CONSTRAINT where the column is unprotected.
  -- Migrations run single-threaded on boot, but the explicit lock makes that
  -- guarantee structural rather than operational.
  LOCK TABLE public.error_events IN ACCESS EXCLUSIVE MODE;

  -- pg_constraint.confdeltype letter codes (PostgreSQL catalog reference):
  --   'a' = NO ACTION  ← the correct state this migration enforces
  --   'r' = RESTRICT
  --   'c' = CASCADE
  --   'n' = SET NULL   ← the incorrect state a previous migration left behind
  --   'd' = SET DEFAULT
  --
  -- Skip DROP+ADD if error_events_api_key_id_fkey already has confdeltype='a'
  -- (NO ACTION). The pg_class join scopes the check to this table specifically.
  IF NOT EXISTS (
    SELECT 1
    FROM   pg_constraint c
    JOIN   pg_class      t ON t.oid = c.conrelid
    JOIN   pg_namespace  n ON n.oid = t.relnamespace
    WHERE  c.conname     = 'error_events_api_key_id_fkey'
      AND  c.contype     = 'f'
      AND  c.confdeltype = 'a'   -- 'a' = NO ACTION (desired state)
      AND  t.relname     = 'error_events'
      AND  n.nspname     = 'public'
  ) THEN
    RAISE NOTICE 'migration 0013: fixing error_events FK from SET NULL to NO ACTION';
    ALTER TABLE public.error_events
      DROP CONSTRAINT IF EXISTS error_events_api_key_id_fkey;
    ALTER TABLE public.error_events
      ADD CONSTRAINT error_events_api_key_id_fkey
      FOREIGN KEY (api_key_id) REFERENCES public.api_keys(id) ON DELETE NO ACTION;
  ELSE
    RAISE NOTICE 'migration 0013: error_events FK already NO ACTION, skipping';
  END IF;
END $$;

COMMIT;
