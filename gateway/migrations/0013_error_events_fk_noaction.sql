-- Repair error_events.api_key_id FK to ON DELETE NO ACTION.
--
-- ON DELETE NO ACTION (confdeltype = 'a') is the desired rule: it preserves the full
-- audit trail by preventing api_keys deletion while error_events rows reference the key.
-- This is NOT ON DELETE SET NULL and NOT ON DELETE CASCADE — the row is kept intact.
--
-- PostgreSQL confdeltype codes: 'a' = NO ACTION (desired), 'r' = RESTRICT,
-- 'c' = CASCADE, 'n' = SET NULL, 'd' = SET DEFAULT.
-- The guard below is a no-op when the constraint already carries ON DELETE NO ACTION.
DO $$
BEGIN
  -- Fail fast rather than blocking the deployment indefinitely if error_events
  -- is actively locked. 30 s statement_timeout covers the full block.
  -- SET (not SET LOCAL) so the timeouts persist for the session, not just
  -- the current subtransaction, in case the migration tool wraps files in
  -- an outer transaction where SET LOCAL would be reverted at commit.
  SET lock_timeout = '10s';
  SET statement_timeout = '30s';

  -- No-op: constraint already has ON DELETE NO ACTION (confdeltype = 'a').
  IF EXISTS (
    SELECT 1 FROM pg_constraint c
    JOIN pg_class r ON r.oid = c.conrelid
    WHERE c.conname     = 'error_events_api_key_id_fkey'
      AND r.relname     = 'error_events'
      AND c.confdeltype = 'a'   -- 'a' = ON DELETE NO ACTION (desired rule)
  ) THEN RETURN; END IF;

  -- Purge orphaned rows (api_key_id non-NULL but the api_keys row is gone)
  -- before re-adding the FK so ADD CONSTRAINT validation cannot fail on stale data.
  DELETE FROM public.error_events
  WHERE api_key_id IS NOT NULL
    AND NOT EXISTS (
      SELECT 1 FROM public.api_keys WHERE id = error_events.api_key_id
    );

  -- Replace any existing constraint (possibly carrying the wrong delete rule,
  -- or absent entirely) with the correct ON DELETE NO ACTION rule.
  ALTER TABLE public.error_events
    DROP CONSTRAINT IF EXISTS error_events_api_key_id_fkey;

  ALTER TABLE public.error_events
    ADD CONSTRAINT error_events_api_key_id_fkey
      FOREIGN KEY (api_key_id) REFERENCES public.api_keys(id)
      ON DELETE NO ACTION;

END $$;
