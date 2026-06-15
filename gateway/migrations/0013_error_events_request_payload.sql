-- Idempotent: ADD COLUMN IF NOT EXISTS is safe to re-run on every gateway boot
-- (migrations run in lexical order with no version table, so every file must be
-- idempotent per the framework invariant in CLAUDE.md).
ALTER TABLE error_events ADD COLUMN IF NOT EXISTS request_payload TEXT;
