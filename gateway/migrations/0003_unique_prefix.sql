-- Ensure each active API key prefix is unique. The non-unique index from 0001 is
-- replaced with a UNIQUE partial index over rows where revoked_at IS NULL.
-- Combined with PrefixLen bumped from 12 → 24 in the application (15 random base32
-- chars of entropy), collisions are now astronomically improbable; the unique index
-- guarantees correctness if one ever happens.

BEGIN;
DROP INDEX IF EXISTS idx_api_keys_active_prefix;
CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_active_prefix_unique
  ON api_keys(prefix)
  WHERE revoked_at IS NULL;
COMMIT;
