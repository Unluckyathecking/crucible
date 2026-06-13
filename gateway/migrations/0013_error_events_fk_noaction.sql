-- Repair error_events.api_key_id FK to ON DELETE NO ACTION.
--
-- ON DELETE NO ACTION (confdeltype = 'a') is the desired rule: it preserves the full
-- audit trail by preventing api_keys deletion while error_events rows reference the key.
-- This is NOT ON DELETE SET NULL and NOT ON DELETE CASCADE — the row is kept intact.
--
-- PostgreSQL confdeltype codes: 'a' = NO ACTION (desired), 'r' = RESTRICT,
-- 'c' = CASCADE, 'n' = SET NULL, 'd' = SET DEFAULT.
-- The guard skips this block when the constraint already has ON DELETE NO ACTION
-- AND is fully validated (convalidated = true), making re-runs a true no-op.
DO $$
BEGIN
  -- SET LOCAL reverts automatically at transaction end so these settings cannot
  -- leak to other queries on a pooled connection. Fail fast on stuck locks.
  SET LOCAL lock_timeout = '10s';
  SET LOCAL statement_timeout = '30s';
  SET LOCAL search_path = public;

  -- No-op: constraint already has ON DELETE NO ACTION and is fully validated.
  IF EXISTS (
    SELECT 1 FROM pg_constraint c
    JOIN pg_class r ON r.oid = c.conrelid
    WHERE c.conname      = 'error_events_api_key_id_fkey'
      AND r.relname      = 'error_events'
      AND c.confdeltype  = 'a'    -- 'a' = ON DELETE NO ACTION (desired rule)
      AND c.convalidated = true   -- NOT VALID constraints have convalidated = false
  ) THEN RETURN; END IF;

  -- Acquire exclusive lock before purging and altering to prevent concurrent inserts
  -- of orphaned rows into the drop/add window.
  LOCK TABLE public.error_events IN ACCESS EXCLUSIVE MODE;

  -- Purge orphaned rows (api_key_id non-NULL but the api_keys row is gone)
  -- before re-adding the FK so VALIDATE CONSTRAINT cannot fail on stale data.
  DELETE FROM public.error_events
  WHERE api_key_id IS NOT NULL
    AND NOT EXISTS (
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
