BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

-- Ensure error_events.api_key_id FK is ON DELETE NO ACTION.
-- DROP CONSTRAINT IF EXISTS + ADD under ACCESS EXCLUSIVE lock is idempotent:
-- it repairs any prior wrong delete rule (CASCADE, SET NULL, RESTRICT, etc.)
-- and is a no-op if the tables do not yet exist.
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'public' AND c.relname = 'error_events' AND c.relkind = 'r'
  ) THEN RETURN; END IF;

  IF NOT EXISTS (
    SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'public' AND c.relname = 'api_keys' AND c.relkind = 'r'
  ) THEN RETURN; END IF;

  -- Lock both tables before touching the FK: ADD CONSTRAINT requires ACCESS
  -- EXCLUSIVE on the referenced table (api_keys) to validate the FK.
  LOCK TABLE public.error_events, public.api_keys IN ACCESS EXCLUSIVE MODE;

  ALTER TABLE public.error_events
    DROP CONSTRAINT IF EXISTS error_events_api_key_id_fkey;

  ALTER TABLE public.error_events
    ADD CONSTRAINT error_events_api_key_id_fkey
      FOREIGN KEY (api_key_id) REFERENCES public.api_keys(id) ON DELETE NO ACTION;
END $$;

COMMIT;
