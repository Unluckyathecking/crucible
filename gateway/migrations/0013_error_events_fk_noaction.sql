BEGIN;

-- Restore error_events.api_key_id FK to NO ACTION.
-- Previous version used ON DELETE SET NULL, which destroyed the audit-log link
-- between an error event and the key that caused it.
-- DROP IF EXISTS + ADD inside a transaction is idempotent: repeated runs are safe
-- because the DROP removes any pre-existing constraint before re-adding it.
ALTER TABLE public.error_events
  DROP CONSTRAINT IF EXISTS error_events_api_key_id_fkey;
ALTER TABLE public.error_events
  ADD CONSTRAINT error_events_api_key_id_fkey
  FOREIGN KEY (api_key_id) REFERENCES public.api_keys(id) ON DELETE NO ACTION;

COMMIT;
