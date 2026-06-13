BEGIN;

-- Restore error_events.api_key_id FK to ON DELETE NO ACTION.
-- A previous migration incorrectly used ON DELETE SET NULL, which destroyed
-- the audit-log link between an error event and the responsible API key.
--
-- Safe for re-runs: execute DROP+ADD only when the correct NO ACTION constraint
-- does not already exist. Not safe for concurrent execution (brief constraint gap
-- between DROP and ADD), but migrations run single-threaded on each boot.
DO $$
BEGIN
  -- Guard: both tables must exist before we touch any constraints.
  -- Avoids the ::regclass cast erroring on a schema where either table is absent.
  IF NOT EXISTS (
    SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'public' AND c.relname = 'error_events' AND c.relkind = 'r'
  ) OR NOT EXISTS (
    SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'public' AND c.relname = 'api_keys' AND c.relkind = 'r'
  ) THEN
    RETURN;
  END IF;

  -- confdeltype='a' is PostgreSQL's catalog code for NO ACTION (the desired
  -- state). If that constraint already exists, skip DROP+ADD.
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
