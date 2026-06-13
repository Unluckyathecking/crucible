-- Repair error_events.api_key_id FK to ON DELETE NO ACTION.
--
-- ON DELETE NO ACTION (confdeltype = 'a') prevents deleting api_keys rows
-- while error_events rows reference them, preserving the full audit trail.
--
-- This migration runs on every gateway boot. The guard returns immediately
-- when the FK is already valid, making the common case a single cheap catalog
-- query. The full repair path acquires ACCESS EXCLUSIVE on both tables before
-- purging orphans: locking api_keys prevents concurrent deletes from creating
-- new orphans between the DELETE and the VALIDATE CONSTRAINT scan.
--
-- PostgreSQL confdeltype codes: 'a' = NO ACTION, 'r' = RESTRICT,
-- 'c' = CASCADE, 'n' = SET NULL, 'd' = SET DEFAULT.
-- convalidated = true means the constraint covers all existing rows.
DO $$
BEGIN
  SET LOCAL lock_timeout = '10s';
  -- 5 minutes: the orphan purge and VALIDATE scan the full table.
  -- lock_timeout governs the ACCESS EXCLUSIVE wait; statement_timeout covers
  -- the total PL/pgSQL block including the VALIDATE scan.
  SET LOCAL statement_timeout = '5min';
  SET LOCAL search_path = public;

  -- Guard: constraint already fully valid with the desired delete rule — skip.
  IF EXISTS (
    SELECT 1
    FROM   pg_constraint c
    JOIN   pg_class      r ON r.oid = c.conrelid
    JOIN   pg_namespace  n ON n.oid = r.relnamespace
    WHERE  c.conname      = 'error_events_api_key_id_fkey'
      AND  r.relname      = 'error_events'
      AND  n.nspname      = 'public'
      AND  c.confdeltype  = 'a'
      AND  c.convalidated = true
  ) THEN RETURN; END IF;

  -- Lock both tables before the orphan purge. Locking api_keys prevents a
  -- concurrent DELETE from api_keys creating a new orphan between the purge
  -- and the VALIDATE CONSTRAINT scan.
  LOCK TABLE public.error_events, public.api_keys IN ACCESS EXCLUSIVE MODE;

  -- Remove orphaned rows: api_key_id is non-NULL but the referenced api_keys
  -- row no longer exists. NULL api_key_id is valid per FK semantics (no
  -- reference) and is preserved by the IS NOT NULL guard.
  DELETE FROM public.error_events
  WHERE  api_key_id IS NOT NULL
    AND  NOT EXISTS (
      SELECT 1 FROM public.api_keys WHERE id = error_events.api_key_id
    );

  -- Three-step pattern: DROP (idempotent), ADD NOT VALID (skips the full-table
  -- scan by relying on the orphan purge above), then VALIDATE.
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
