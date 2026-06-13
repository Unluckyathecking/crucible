-- Repair error_events.api_key_id FK to ON DELETE NO ACTION.
-- ON DELETE NO ACTION enforces referential integrity by preventing deletion
-- of api_keys rows that error_events rows still reference.
--
-- Idempotent: the guard exits immediately when the FK is already correct,
-- making the common case a single cheap catalog query with no locking.
-- Fully qualified table names (public.*) guard against search_path surprises.
DO $$
BEGIN
  -- Skip the repair if the FK already exists with ON DELETE NO ACTION and is
  -- validated (covers all existing rows). confdeltype='a' is PostgreSQL's
  -- internal code for ON DELETE NO ACTION — the same rule we ADD below.
  IF EXISTS (
    SELECT 1
    FROM   pg_constraint c
    JOIN   pg_class      r ON r.oid = c.conrelid
    JOIN   pg_namespace  n ON n.oid = r.relnamespace
    WHERE  c.conname      = 'error_events_api_key_id_fkey'
      AND  r.relname      = 'error_events'
      AND  n.nspname      = 'public'
      AND  c.confdeltype  = 'a'     -- 'a' = ON DELETE NO ACTION
      AND  c.convalidated = true
  ) THEN RETURN; END IF;

  -- Lock both tables before the orphan scan to prevent a concurrent api_keys
  -- DELETE from creating a new orphan between the purge and FK validation.
  LOCK TABLE public.error_events, public.api_keys IN ACCESS EXCLUSIVE MODE;

  -- Purge rows where api_key_id is non-NULL but the referenced api_keys row no
  -- longer exists. Such rows cannot satisfy ON DELETE NO ACTION and must be
  -- removed before the constraint can be added. NULL api_key_id rows are left
  -- untouched (the WHERE requires IS NOT NULL).
  -- NOT EXISTS with explicit table aliases: e = error_events row being evaluated,
  -- k = the api_keys row we look for. If no api_keys row with k.id = e.api_key_id
  -- exists, the error_events row is an orphan and must be deleted before the FK
  -- can be added. Using NOT EXISTS (rather than NOT IN) avoids the NULL-in-subquery
  -- footgun and lets the planner use an index on api_keys.id.
  DELETE FROM public.error_events e
  WHERE  e.api_key_id IS NOT NULL
    AND  NOT EXISTS (SELECT 1 FROM public.api_keys k WHERE k.id = e.api_key_id);

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
