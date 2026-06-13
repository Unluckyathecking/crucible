BEGIN;

-- Restore error_events.api_key_id FK to ON DELETE NO ACTION.
-- A previous migration incorrectly used ON DELETE SET NULL, which destroyed
-- the audit-log link between an error event and the responsible API key.
--
-- Re-run behaviour: the DO block skips the fix when the correct NO ACTION
-- constraint (confdeltype='a') already exists. The migration runner is
-- single-threaded, so concurrent execution is not a production concern;
-- the LOCK TABLE inside the fix branch makes that guarantee structural.
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

  -- pg_constraint.confdeltype letter codes (PostgreSQL catalog reference):
  --   'a' = NO ACTION  ← the correct state this migration enforces
  --   'r' = RESTRICT
  --   'c' = CASCADE
  --   'n' = SET NULL   ← the incorrect state a previous migration left behind
  --   'd' = SET DEFAULT
  --
  -- Catalog read is lock-free. The heavy lock is only acquired when a fix is
  -- actually needed, avoiding unnecessary blocking on already-correct schemas.
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
    -- Acquire ACCESS EXCLUSIVE only when work is needed (avoids unnecessary
    -- blocking when the constraint is already correct).
    LOCK TABLE public.error_events IN ACCESS EXCLUSIVE MODE;
    RAISE NOTICE 'migration 0013: fixing error_events FK from SET NULL to NO ACTION';
    -- Single ALTER TABLE with both subcommands: PostgreSQL executes DROP and ADD
    -- in the same DDL statement, so there is no window where the column has no
    -- FK constraint.
    ALTER TABLE public.error_events
      DROP CONSTRAINT IF EXISTS error_events_api_key_id_fkey,
      ADD CONSTRAINT error_events_api_key_id_fkey
        FOREIGN KEY (api_key_id) REFERENCES public.api_keys(id) ON DELETE NO ACTION;
  ELSE
    RAISE NOTICE 'migration 0013: error_events FK already NO ACTION, skipping';
  END IF;
END $$;

COMMIT;
