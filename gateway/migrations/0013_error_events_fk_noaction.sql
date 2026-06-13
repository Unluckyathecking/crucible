-- Repair error_events.api_key_id FK to ON DELETE NO ACTION.
--
-- ON DELETE NO ACTION prevents deleting api_keys rows while error_events rows
-- reference them, preserving the full audit trail.
--
-- Runs on every gateway boot; the guard returns immediately when the FK is
-- already fully valid, making the common case a single cheap catalog query.
-- All table names are fully qualified (public.*) to avoid search_path dependency.
--
-- PostgreSQL confdeltype codes: 'a' = NO ACTION, 'r' = RESTRICT,
-- 'c' = CASCADE, 'n' = SET NULL, 'd' = SET DEFAULT.
-- convalidated = true means the constraint covers all existing rows.
DO $$
BEGIN
  -- Guard: skip if already fully valid with ON DELETE NO ACTION.
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
  -- and the ADD CONSTRAINT validation scan.
  LOCK TABLE public.error_events, public.api_keys IN ACCESS EXCLUSIVE MODE;

  -- Remove orphaned rows: api_key_id non-NULL but the referenced api_keys row
  -- no longer exists. NULL api_key_id is valid per FK semantics (no reference)
  -- and is preserved by the IS NOT NULL guard.
  DELETE FROM public.error_events
  WHERE  api_key_id IS NOT NULL
    AND  NOT EXISTS (
      SELECT 1 FROM public.api_keys WHERE id = error_events.api_key_id
    );

  -- Drop any existing FK (regardless of its current delete rule) then recreate
  -- with ON DELETE NO ACTION. ADD CONSTRAINT validates existing rows inline
  -- under the ACCESS EXCLUSIVE lock, so no separate VALIDATE step is needed.
  ALTER TABLE public.error_events
    DROP CONSTRAINT IF EXISTS error_events_api_key_id_fkey;

  ALTER TABLE public.error_events
    ADD CONSTRAINT error_events_api_key_id_fkey
      FOREIGN KEY (api_key_id) REFERENCES public.api_keys(id)
      ON DELETE NO ACTION;

END $$;
