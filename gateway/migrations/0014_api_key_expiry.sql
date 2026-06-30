-- Idempotent: re-runs safely on every gateway boot (invariant #8).
-- Adds nullable grace-period expiry to api_keys for zero-downtime key rotation.
-- A NULL expires_at means the key never expires. A future expires_at means the key
-- is in its rotation grace window and remains valid until that timestamp.

ALTER TABLE api_keys
  ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ;

-- Partial index: only indexes non-revoked keys that have an expiry set.
-- Supports efficient expiry-sweep queries without bloating the index with NULL rows.
CREATE INDEX IF NOT EXISTS idx_api_keys_expires_at
  ON api_keys (expires_at)
  WHERE revoked_at IS NULL AND expires_at IS NOT NULL;
