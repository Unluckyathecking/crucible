#!/usr/bin/env bash
# Seed a test customer + API key into the local Crucible database.
#
# Reads POSTGRES_DSN, API_KEY_HASH_SALT, API_KEY_PREFIX from the environment
# (or .env in the current directory). Idempotent.
#
# Usage:  scripts/seed-dev.sh
# Prints the full key on stdout — use it as Authorization: Bearer <key>.
set -euo pipefail

# Source .env if present (best-effort; ignore parse errors on comments).
if [[ -f .env ]]; then
  set -a
  # shellcheck disable=SC1091
  source .env
  set +a
fi

DSN="${POSTGRES_DSN:?POSTGRES_DSN must be set}"
SALT="${API_KEY_HASH_SALT:?API_KEY_HASH_SALT must be set}"
PREFIX="${API_KEY_PREFIX:-cru_}"

# Deterministic dev key (24 'A' bytes worth of fake entropy → base32 nopadding).
# Real keys come from auth.Generate(); this one exists so curl examples in README work.
FULL_KEY="${PREFIX}live_IFAUCQKBIFAUCQKBIFAUCQKBIFAUCQKBIFAUCQKB"
# Must match PrefixLen (24) in gateway/internal/auth/keys.go.
DISPLAY_PREFIX="${FULL_KEY:0:24}"
# shasum (macOS) vs sha256sum (Linux) — pick whichever is on PATH.
if command -v shasum >/dev/null 2>&1; then
  HASH=$(printf '%s%s' "$SALT" "$FULL_KEY" | shasum -a 256 | awk '{print $1}')
else
  HASH=$(printf '%s%s' "$SALT" "$FULL_KEY" | sha256sum | awk '{print $1}')
fi

psql "$DSN" <<EOF >/dev/null
INSERT INTO customers (id, email, plan_id) VALUES
  ('00000000-0000-0000-0000-000000000001', 'dev@example.com', 'pro')
ON CONFLICT (email) DO NOTHING;

INSERT INTO api_keys (id, customer_id, prefix, hash, name) VALUES
  ('00000000-0000-0000-0000-000000000002',
   '00000000-0000-0000-0000-000000000001',
   '$DISPLAY_PREFIX',
   decode('$HASH', 'hex'),
   'dev')
ON CONFLICT (id) DO NOTHING;
EOF

echo "Seeded customer dev@example.com (plan=pro)."
echo
echo "Dev key (use as Authorization: Bearer):"
echo "  $FULL_KEY"
echo
echo "Smoke test:"
echo "  curl -s -X POST localhost:8080/v1/echo \\"
echo "    -H 'authorization: Bearer $FULL_KEY' \\"
echo "    -H 'content-type: application/json' \\"
echo "    -d '{\"x\":\"hi\"}' | jq"
