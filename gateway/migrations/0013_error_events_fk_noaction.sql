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
  -- Guard: error_events table must exist.
  IF NOT EXISTS (
    SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'public' AND c.relname = 'error_events' AND c.relkind = 'r'
  ) THEN
    RETURN;
  END IF;

  -- Guard: api_keys table must exist (referenced by the FK we are adding).
  IF NOT EXISTS (
    SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'public' AND c.relname = 'api_keys' AND c.relkind = 'r'
  ) THEN
    RETURN;
  END IF;

  -- confdeltype='a' is PostgreSQL's catalog code for NO ACTION (the desired
  -- state). Skip DROP+ADD if the correct constraint already exists.
  -- Include table scoping via pg_class join so the check is unambiguous even if
  -- another table coincidentally carries a constraint with the same name.
  -- Safe to use here: both tables verified above, so pg_class rows exist.
  IF NOT EXISTS (
    SELECT 1
    FROM   pg_constraint c
    JOIN   pg_class      t ON t.oid = c.conrelid
    JOIN   pg_namespace  n ON n.oid = t.relnamespace
    WHERE  c.conname     = 'error_events_api_key_id_fkey'
      AND  c.contype     = 'f'
      AND  c.confdeltype = 'a'
      AND  t.relname     = 'error_events'
      AND  n.nspname     = 'public'
  ) THEN
    ALTER TABLE public.error_events
      DROP CONSTRAINT IF EXISTS error_events_api_key_id_fkey;
    ALTER TABLE public.error_events
      ADD CONSTRAINT error_events_api_key_id_fkey
      FOREIGN KEY (api_key_id) REFERENCES public.api_keys(id) ON DELETE NO ACTION;
  END IF;
END $$;

COMMIT;
