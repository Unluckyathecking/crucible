-- Repair error_events.api_key_id FK to ON DELETE NO ACTION.
--
-- ON DELETE NO ACTION (confdeltype = 'a') is the desired rule: it preserves the full
-- audit trail by preventing api_keys deletion while error_events rows reference the key.
-- This is NOT ON DELETE SET NULL and NOT ON DELETE CASCADE — the row is kept intact.
--
-- PostgreSQL confdeltype codes: 'a' = NO ACTION (desired), 'r' = RESTRICT,
-- 'c' = CASCADE, 'n' = SET NULL, 'd' = SET DEFAULT.
--
-- Three guards, in order:
--   1. Fully-valid constraint: confdeltype = 'a' AND convalidated = true  → RETURN (true no-op)
--   2. NOT VALID constraint:   confdeltype = 'a' AND convalidated = false → VALIDATE and RETURN
--      (handles interrupted runs that completed ADD but not VALIDATE)
--   3. Neither: DROP, ADD NOT VALID, VALIDATE (full repair path)
--
-- The pg_namespace join (n.nspname = 'public') guards against name collisions in
-- multi-schema databases where another schema might have a same-named table/constraint.
DO $$
BEGIN
  -- SET LOCAL reverts automatically at transaction end so these settings cannot
  -- leak to other queries on a pooled connection.
  SET LOCAL lock_timeout = '10s';
  -- 5 minutes: the orphan purge and VALIDATE CONSTRAINT scan the full table; 30 s
  -- is too tight on large datasets. lock_timeout governs the ACCESS EXCLUSIVE wait.
  SET LOCAL statement_timeout = '5min';
  SET LOCAL search_path = public;

  -- Guard 1: constraint already fully valid with the desired rule — skip everything.
  IF EXISTS (
    SELECT 1
    FROM   pg_constraint c
    JOIN   pg_class      r ON r.oid = c.conrelid
    JOIN   pg_namespace  n ON n.oid = r.relnamespace
    WHERE  c.conname      = 'error_events_api_key_id_fkey'
      AND  r.relname      = 'error_events'
      AND  n.nspname      = 'public'
      AND  c.confdeltype  = 'a'    -- 'a' = ON DELETE NO ACTION
      AND  c.convalidated = true   -- fully validated (NOT VALID → false)
  ) THEN RETURN; END IF;

  -- Guard 2: ADD NOT VALID succeeded but VALIDATE was interrupted or failed.
  -- VALIDATE may have failed because orphaned rows existed at the time; simply
  -- re-running VALIDATE without purging them would fail again. Lock both tables,
  -- purge any remaining orphans, then retry VALIDATE.
  IF EXISTS (
    SELECT 1
    FROM   pg_constraint c
    JOIN   pg_class      r ON r.oid = c.conrelid
    JOIN   pg_namespace  n ON n.oid = r.relnamespace
    WHERE  c.conname      = 'error_events_api_key_id_fkey'
      AND  r.relname      = 'error_events'
      AND  n.nspname      = 'public'
      AND  c.confdeltype  = 'a'
      AND  c.convalidated = false
  ) THEN
    LOCK TABLE public.error_events, public.api_keys IN ACCESS EXCLUSIVE MODE;
    DELETE FROM public.error_events
    WHERE  api_key_id IS NOT NULL
      AND  NOT EXISTS (
        SELECT 1 FROM public.api_keys WHERE id = error_events.api_key_id
      );
    ALTER TABLE public.error_events
      VALIDATE CONSTRAINT error_events_api_key_id_fkey;
    RETURN;
  END IF;

  -- Full repair path: acquire exclusive lock, purge orphans, recreate the FK.
  -- ACCESS EXCLUSIVE prevents concurrent inserts of orphaned rows between the
  -- DROP and ADD steps.
  -- Lock both tables: locking only error_events leaves a window where a concurrent
  -- transaction could delete an api_keys row between the orphan purge and the
  -- VALIDATE CONSTRAINT scan, creating a new orphan that fails validation.
  LOCK TABLE public.error_events, public.api_keys IN ACCESS EXCLUSIVE MODE;

  -- Purge orphaned rows (api_key_id non-NULL but the api_keys row is gone)
  -- before re-adding the FK so VALIDATE CONSTRAINT cannot fail on stale data.
  -- The WHERE clause is api_key_id IS NOT NULL (skip NULLs, which are valid per
  -- the FK semantics) AND the referenced row is absent.
  DELETE FROM public.error_events
  WHERE  api_key_id IS NOT NULL
    AND  NOT EXISTS (
      SELECT 1 FROM public.api_keys WHERE id = error_events.api_key_id
    );

  -- Three-step pattern: DROP, ADD NOT VALID, then VALIDATE.
  -- NOT VALID skips the full-table scan on ADD, relying on the orphan purge above.
  -- VALIDATE then confirms all existing rows satisfy the constraint.
  ALTER TABLE public.error_events
    DROP CONSTRAINT IF EXISTS error_events_api_key_id_fkey;

  ALTER TABLE public.error_events
    ADD CONSTRAINT error_events_api_key_id_fkey
      FOREIGN KEY (api_key_id) REFERENCES public.api_keys(id)
      ON DELETE NO ACTION
      NOT VALID;

  ALTER TABLE public.error_events
    VALIDATE CONSTRAINT error_events_api_key_id_fkey;

END $$;
