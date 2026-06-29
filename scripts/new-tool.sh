#!/usr/bin/env bash
# new-tool.sh — clone Crucible into a new product directory.
#
# Usage:  scripts/new-tool.sh <product-name> [<destination-dir>]
#   product-name: kebab-case (e.g. vat-check)
#   destination:  defaults to ../<product-name>
#
# Renames every "crucible" identifier to the product name across the tree, updates Go
# module paths, generates a fresh API_KEY_HASH_SALT and prefix, and initialises a
# clean git repo. Does NOT create a GitHub remote — that's a manual final step after
# the local build passes the acceptance bar.

set -euo pipefail

PRODUCT="${1:?usage: $0 <product-name> [<destination-dir>]}"
DEST="${2:-../$PRODUCT}"

if [[ ! "$PRODUCT" =~ ^[a-z][a-z0-9-]*$ ]]; then
  echo "Product name must be kebab-case: lowercase letters, digits, hyphens (e.g. vat-check)." >&2
  exit 1
fi

if [[ -e "$DEST" ]]; then
  echo "Destination $DEST already exists. Aborting." >&2
  exit 1
fi

SRC=$(cd -- "$(dirname -- "$0")/.." && pwd)

# Portable in-place sed: macOS requires `-i ''`, Linux uses `-i` alone.
sed_in_place() {
  if [[ "$(uname)" == "Darwin" ]]; then
    sed -i '' "$@"
  else
    sed -i "$@"
  fi
}

cp -R "$SRC" "$DEST"
cd "$DEST"
rm -rf .git node_modules dashboard/node_modules dashboard/.next .opencode .vscode

PRODUCT_UNDER=$(echo "$PRODUCT" | tr - _)
PRODUCT_FIRST=$(echo "$PRODUCT" | cut -d- -f1)
PRODUCT_TITLE=$(echo "$PRODUCT" | awk -F- '{for (i=1;i<=NF;i++) printf "%s%s", toupper(substr($i,1,1)) tolower(substr($i,2)), (i<NF)?" ":""}')

# Rename identifiers across all relevant text files.
# Use `find -exec ... +` directly (avoids bash 4 dependency on mapfile).
FIND_ARGS=(. -type f \(
  -name '*.go' -o -name '*.ts' -o -name '*.tsx' -o -name '*.json' -o -name '*.md'
  -o -name '*.yml' -o -name '*.yaml' -o -name '*.sql' -o -name '*.sh'
  -o -name 'Dockerfile' -o -name 'Makefile' -o -name 'go.work' -o -name 'go.mod'
  -o -name '.env.example'
\)
  ! -path './node_modules/*' ! -path './.next/*' ! -path '*/node_modules/*' ! -path '*/.next/*')

SED_EXPRS=(
  -e "s|github.com/Unluckyathecking/crucible|github.com/Unluckyathecking/${PRODUCT_UNDER}|g"
  -e "s|crucible-dashboard|${PRODUCT}-dashboard|g"
  -e "s|Crucible|${PRODUCT_TITLE}|g"
  -e "s|crucible|${PRODUCT_UNDER}|g"
)

if [[ "$(uname)" == "Darwin" ]]; then
  find "${FIND_ARGS[@]}" -exec sed -i '' "${SED_EXPRS[@]}" {} +
else
  find "${FIND_ARGS[@]}" -exec sed -i "${SED_EXPRS[@]}" {} +
fi

# Per-product API key prefix uses the first kebab segment.
# Apply to BOTH the gateway's .env.example and the dashboard's — the dashboard
# issues keys and the gateway verifies them, so the prefix + salt MUST match.
for env_file in .env.example dashboard/.env.example; do
  sed_in_place "s|API_KEY_PREFIX=cru_|API_KEY_PREFIX=${PRODUCT_FIRST}_|" "$env_file"
  sed_in_place "s|cru_units|${PRODUCT_FIRST}_units|" "$env_file"
done

# Generate a single fresh API_KEY_HASH_SALT — used in BOTH env files so they stay in sync.
FRESH_SALT=$(openssl rand -base64 32)
for env_file in .env.example dashboard/.env.example; do
  sed_in_place "s|REPLACE_WITH_32_PLUS_BYTES_OF_RANDOM_DATA__REPLACE_ME|${FRESH_SALT}|" "$env_file"
done

# Init a fresh git repo, no remote.
git init -q -b main
git add -A
git -c user.email=template@local -c user.name="template" commit -q -m "Initial commit (cloned from crucible)" || true

echo "==> running preflight doctor on $DEST"
if ! bash "$DEST/scripts/doctor.sh" "$DEST"; then
  echo "WARNING: doctor detected issues — see DOCTOR_FAIL lines above. Fix before running 'make dev'." >&2
fi

echo
echo "Done. Your new product lives at $DEST"
echo
echo "Next:"
echo "  cd $DEST"
echo "  cp .env.example .env"
echo "  # Edit workers/active to point at your product worker."
echo "  # Add a route per endpoint in gateway/internal/server/routes_table.go."
echo "  # Add plans in gateway/migrations/0003_seed_plans.sql."
echo "  # Re-run scripts/doctor.sh after each adapt step to verify config."
echo "  make dev"
