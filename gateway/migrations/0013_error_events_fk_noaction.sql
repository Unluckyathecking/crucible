BEGIN;

-- Restore error_events.api_key_id FK to ON DELETE NO ACTION.
-- A previous migration incorrectly used ON DELETE SET NULL, which destroyed
-- the audit-log link between an error event and the responsible API key.
--
-- Idempotency: execute DROP+ADD only when the correct NO ACTION constraint
-- does not already exist. confdeltype='a' is PostgreSQL's catalog code for
-- NO ACTION. If the constraint is absent or has any other delete action,
-- this block drops and recreates it with the correct ON DELETE NO ACTION.
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM   pg_constraint
    WHERE  conname     = 'error_events_api_key_id_fkey'
      AND  contype     = 'f'
      AND  conrelid    = 'public.error_events'::regclass
      AND  confdeltype = 'a'
  ) THEN
    ALTER TABLE public.error_events
      DROP CONSTRAINT IF EXISTS error_events_api_key_id_fkey;
    ALTER TABLE public.error_events
      ADD CONSTRAINT error_events_api_key_id_fkey
      FOREIGN KEY (api_key_id) REFERENCES public.api_keys(id) ON DELETE NO ACTION;
  END IF;
END $$;

COMMIT;
