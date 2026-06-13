BEGIN;

-- Restore error_events.api_key_id FK to ON DELETE NO ACTION.
-- Previous version used ON DELETE SET NULL, which destroyed the audit-log
-- link between an error event and the key that caused it.
--
-- Idempotency via confdeltype: pg_constraint.confdeltype stores the delete action
-- as a single char ('a'=NO ACTION, 'n'=SET NULL, 'c'=CASCADE, 'r'=RESTRICT,
-- 'd'=SET DEFAULT). Using confdeltype is more reliable than parsing
-- pg_get_constraintdef text, which may or may not include "ON DELETE NO ACTION"
-- depending on whether the clause was written explicitly.
-- COALESCE(deltype,'a') treats a missing constraint (NULL) the same as NO ACTION
-- for the outer check; the real guard is def IS NULL (constraint absent).
DO $$
DECLARE
  def     TEXT;
  deltype "char";
BEGIN
  SELECT pg_get_constraintdef(oid), confdeltype INTO def, deltype
  FROM   pg_constraint
  WHERE  conname  = 'error_events_api_key_id_fkey'
    AND  contype  = 'f'
    AND  conrelid = 'public.error_events'::regclass;

  -- Skip when constraint already exists with NO ACTION ('a' = default).
  IF def IS NULL OR COALESCE(deltype, 'a') <> 'a' THEN
    ALTER TABLE public.error_events
      DROP CONSTRAINT IF EXISTS error_events_api_key_id_fkey;
    ALTER TABLE public.error_events
      ADD CONSTRAINT error_events_api_key_id_fkey
      FOREIGN KEY (api_key_id) REFERENCES public.api_keys(id) ON DELETE NO ACTION;
  END IF;
END $$;

COMMIT;
