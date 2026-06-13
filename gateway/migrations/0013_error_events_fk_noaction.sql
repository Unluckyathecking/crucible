-- Repair error_events.api_key_id FK to ON DELETE NO ACTION.
-- ON DELETE NO ACTION enforces referential integrity by preventing deletion
-- of api_keys rows that error_events rows still reference.
--
-- Idempotent: the guard exits immediately when the FK already exists with
-- ON DELETE NO ACTION, making the common path a single cheap catalog query.
-- Fully qualified table names (public.*) guard against search_path surprises.
DO $$
BEGIN
  -- Skip the repair if the FK already exists with ON DELETE NO ACTION.
  -- confdeltype='a' is PostgreSQL's internal code for ON DELETE NO ACTION.
  IF EXISTS (
    SELECT 1
    FROM   pg_constraint c
    JOIN   pg_class      r ON r.oid = c.conrelid
    JOIN   pg_namespace  n ON n.oid = r.relnamespace
    WHERE  c.conname      = 'error_events_api_key_id_fkey'
      AND  r.relname      = 'error_events'
      AND  n.nspname      = 'public'
      AND  c.confdeltype  = 'a'     -- 'a' = ON DELETE NO ACTION
  ) THEN RETURN; END IF;

  -- Bound the time this repair blocks concurrent traffic. The guard above
  -- exits immediately on subsequent runs; timeouts only apply on first deployment.
  SET LOCAL lock_timeout      = '30s';
  SET LOCAL statement_timeout = '120s';

  -- Lock both tables before the orphan scan to prevent a concurrent api_keys
  -- DELETE from creating a new orphan between the purge and FK validation.
  LOCK TABLE public.error_events, public.api_keys IN ACCESS EXCLUSIVE MODE;

  -- Purge rows where api_key_id is non-NULL but the referenced api_keys row no
  -- longer exists. Such rows cannot satisfy ON DELETE NO ACTION and must be
  -- removed before the constraint can be added. NULL api_key_id rows are left
  -- untouched by the WHERE ee.api_key_id IS NOT NULL guard.
  -- LEFT JOIN anti-join: rows where the join produces no match (k.id IS NULL)
  -- are the orphans. The ACCESS EXCLUSIVE lock already prevents new orphans
  -- from arriving during the purge, so no LIMIT loop is needed.
  DELETE FROM public.error_events
  WHERE  id IN (
           SELECT ee.id
           FROM   public.error_events  ee
           LEFT   JOIN public.api_keys k ON k.id = ee.api_key_id
           WHERE  ee.api_key_id IS NOT NULL
             AND  k.id IS NULL
         );

  -- Drop any existing FK regardless of its current delete rule, then recreate
  -- it with ON DELETE NO ACTION. ADD CONSTRAINT validates all rows inline under
  -- the ACCESS EXCLUSIVE lock, so no separate VALIDATE step is needed.
  ALTER TABLE public.error_events
    DROP CONSTRAINT IF EXISTS error_events_api_key_id_fkey;

  ALTER TABLE public.error_events
    ADD CONSTRAINT error_events_api_key_id_fkey
      FOREIGN KEY (api_key_id) REFERENCES public.api_keys(id)
      ON DELETE NO ACTION;
END $$;
