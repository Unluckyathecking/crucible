BEGIN;

-- Repair error_events.api_key_id FK to ON DELETE NO ACTION.
--
-- PostgreSQL confdeltype codes: 'a' = NO ACTION (desired), 'r' = RESTRICT,
-- 'c' = CASCADE, 'n' = SET NULL, 'd' = SET DEFAULT.
-- This migration ensures confdeltype = 'a' (NO ACTION): the database blocks
-- deletion of an api_keys row while error_events rows still reference it.
-- Nothing is ever silently nullified or cascaded. The migration is a no-op
-- when the constraint already carries the correct rule.
--
-- lock_timeout limits DDL lock-acquisition time; set at the transaction level
-- so it covers all ALTER TABLE statements in this migration.
SET LOCAL lock_timeout = '5000';

DO $$
BEGIN
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

COMMIT;
