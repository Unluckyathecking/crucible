-- Idempotent: ADD COLUMN IF NOT EXISTS is safe to re-run on every gateway boot
-- (migrations run in lexical order with no version table, so every file must be
-- idempotent per the framework invariant in CLAUDE.md).
-- BYTEA stores the raw request bytes without encoding assumptions; TEXT would
-- reject non-UTF-8 bodies at the PostgreSQL layer.
ALTER TABLE error_events ADD COLUMN IF NOT EXISTS request_payload BYTEA;
