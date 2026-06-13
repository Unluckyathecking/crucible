BEGIN;

-- Restore error_events.api_key_id FK to ON DELETE NO ACTION.
-- Previous version used ON DELETE SET NULL, which destroyed the audit-log
-- link between an error event and the key that caused it.
--
-- Idempotency via pg_get_constraintdef: PostgreSQL omits the ON DELETE clause
-- from the definition text when the action is NO ACTION (the default). Any other
-- ON DELETE action (SET NULL, CASCADE, etc.) appears explicitly in the output.
-- The DO block skips DROP+ADD when the constraint already has correct semantics;
-- it also handles fresh schemas where def IS NULL (constraint absent).
DO $$
DECLARE
  def TEXT;
BEGIN
  SELECT pg_get_constraintdef(oid) INTO def
  FROM   pg_constraint
  WHERE  conname  = 'error_events_api_key_id_fkey'
    AND  contype  = 'f'
    AND  conrelid = 'public.error_events'::regclass;

  IF def IS NULL OR def LIKE '%ON DELETE%' THEN
    ALTER TABLE public.error_events
      DROP CONSTRAINT IF EXISTS error_events_api_key_id_fkey;
    ALTER TABLE public.error_events
      ADD CONSTRAINT error_events_api_key_id_fkey
      FOREIGN KEY (api_key_id) REFERENCES public.api_keys(id) ON DELETE NO ACTION;
  END IF;
END $$;

COMMIT;
