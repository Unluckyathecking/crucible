BEGIN;

-- Restore error_events.api_key_id FK to NO ACTION.
--
-- An earlier version used ON DELETE SET NULL to allow the test harness cleanup
-- to delete api_keys before error_events. That was reverted: SET NULL on an
-- audit-log FK destroys the link between an error event and the key that caused it.
-- The harness cleanup now deletes error_events before api_keys to satisfy the FK.
--
-- Idempotent via DROP IF EXISTS + ADD: if the constraint is absent or has the wrong
-- delete action, this restores it. If it already has NO ACTION semantics, the DROP
-- removes it and ADD re-creates it within the same transaction — the brief absence
-- is invisible to concurrent readers outside the transaction boundary.
ALTER TABLE public.error_events
  DROP CONSTRAINT IF EXISTS error_events_api_key_id_fkey;
ALTER TABLE public.error_events
  ADD CONSTRAINT error_events_api_key_id_fkey
  FOREIGN KEY (api_key_id) REFERENCES public.api_keys(id) ON DELETE NO ACTION;

COMMIT;
