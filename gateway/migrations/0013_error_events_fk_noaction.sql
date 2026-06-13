BEGIN;

-- Restore error_events.api_key_id FK to ON DELETE NO ACTION.
-- A previous migration incorrectly used ON DELETE SET NULL, which destroyed
-- the audit-log link between an error event and the responsible API key.
--
-- Re-run behaviour: the DO block is idempotent via three guards:
--   1. Skip entirely on fresh DBs that never had the error_events table.
--   2. Skip the ADD if the temp constraint already exists (partial previous run).
--   3. Skip the whole fix if the final constraint is already NO ACTION.
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

    -- Phase 1: add the replacement constraint as NOT VALID so no table scan
    -- is needed under the lock, and there is no window with zero FK coverage.
    -- Clean up any leftover temp constraint from a partial previous run first.
    ALTER TABLE public.error_events
      DROP CONSTRAINT IF EXISTS error_events_api_key_id_fkey_new;
    ALTER TABLE public.error_events
      ADD CONSTRAINT error_events_api_key_id_fkey_new
        FOREIGN KEY (api_key_id) REFERENCES public.api_keys(id) ON DELETE NO ACTION NOT VALID;

    -- Phase 2: validate the new constraint against existing rows.
    -- VALIDATE CONSTRAINT acquires ShareUpdateExclusiveLock (not AccessExclusive),
    -- so concurrent reads are not blocked during the scan.
    ALTER TABLE public.error_events
      VALIDATE CONSTRAINT error_events_api_key_id_fkey_new;

    -- Phase 3: drop the old (incorrect) constraint, then rename the new one to
    -- the canonical name. Both steps execute under the existing ACCESS EXCLUSIVE
    -- lock, so no concurrent writer can observe the intermediate state.
    ALTER TABLE public.error_events
      DROP CONSTRAINT IF EXISTS error_events_api_key_id_fkey;
    ALTER TABLE public.error_events
      RENAME CONSTRAINT error_events_api_key_id_fkey_new TO error_events_api_key_id_fkey;
  ELSE
    RAISE NOTICE 'migration 0013: error_events FK already NO ACTION, skipping';
  END IF;
END $$;

COMMIT;
