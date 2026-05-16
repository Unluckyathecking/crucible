#!/usr/bin/env bash
# smoke-new-tool.sh — verify that the clone-and-rename script produces a buildable tree.
#
# This is the LOAD-BEARING test of the framework's value proposition: if a developer
# can't clone and have something compile, the entire framework is dead. Run on every PR
# (via .github/workflows/new-tool-smoke.yml) AND locally (via `make smoke-test-new-tool`).
set -euo pipefail

REPO=$(cd -- "$(dirname -- "$0")/.." && pwd)
WORK=$(mktemp -d -t crucible-smoke-XXXXXX)
trap 'rm -rf "$WORK"' EXIT

echo "==> Running new-tool.sh into $WORK/demo-clone"
"$REPO/scripts/new-tool.sh" demo-clone "$WORK/demo-clone" >/dev/null

cd "$WORK/demo-clone"

echo "==> go work sync"
go work sync

echo "==> building all Go modules"
(cd workers/sdk-go && go build ./... && go vet ./...)
(cd workers/stubs/go && go build ./... && go vet ./...)
(cd gateway && go build ./... && go vet ./...)

echo "==> sanity: identifier rename happened"
if grep -rq "Unluckyathecking/crucible" --include='*.go' --include='go.mod' .; then
  echo "FAIL: 'Unluckyathecking/crucible' still appears in cloned tree" >&2
  grep -rn "Unluckyathecking/crucible" --include='*.go' --include='go.mod' . >&2
  exit 1
fi
if ! grep -rq "Unluckyathecking/demo_clone" --include='go.mod' .; then
  echo "FAIL: 'Unluckyathecking/demo_clone' not found after rename" >&2
  exit 1
fi

echo "==> sanity: gateway and dashboard env share the same API key prefix and salt"
ROOT_PREFIX=$(grep '^API_KEY_PREFIX=' .env.example | cut -d= -f2)
DASH_PREFIX=$(grep '^API_KEY_PREFIX=' dashboard/.env.example | cut -d= -f2)
if [[ "$ROOT_PREFIX" != "$DASH_PREFIX" ]]; then
  echo "FAIL: API_KEY_PREFIX mismatch — root='$ROOT_PREFIX' dashboard='$DASH_PREFIX'" >&2
  exit 1
fi
ROOT_SALT=$(grep '^API_KEY_HASH_SALT=' .env.example | cut -d= -f2)
DASH_SALT=$(grep '^API_KEY_HASH_SALT=' dashboard/.env.example | cut -d= -f2)
if [[ "$ROOT_SALT" != "$DASH_SALT" || "$ROOT_SALT" == "REPLACE_WITH"* ]]; then
  echo "FAIL: API_KEY_HASH_SALT mismatch or placeholder not replaced" >&2
  exit 1
fi

echo
echo "smoke-new-tool: PASS"
