BEGIN;

-- Restore error_events.api_key_id FK to NO ACTION.
--
-- An earlier version used ON DELETE SET NULL to allow the test harness cleanup
-- to delete api_keys before error_events. That was reverted: SET NULL on an
-- audit-log FK destroys the link between an error event and the key that caused it.
-- The harness cleanup now deletes error_events before api_keys to satisfy the FK.
--
-- The DO block makes this idempotent: it only drops and re-creates the constraint
-- when the existing constraint does not already have NO ACTION semantics
-- (confdeltype = 'a'). Skipping the DROP/ADD on an already-correct constraint
-- avoids briefly removing the FK and lets concurrent readers see a consistent state.
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname     = 'error_events_api_key_id_fkey'
      AND contype     = 'f'
      AND conrelid    = 'public.error_events'::regclass
      AND confdeltype = 'a'
  ) THEN
    ALTER TABLE public.error_events
      DROP CONSTRAINT IF EXISTS error_events_api_key_id_fkey;
    ALTER TABLE public.error_events
      ADD CONSTRAINT error_events_api_key_id_fkey
      FOREIGN KEY (api_key_id) REFERENCES public.api_keys(id) ON DELETE NO ACTION;
  END IF;
END $$;

COMMIT;
