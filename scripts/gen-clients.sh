#!/usr/bin/env bash
# gen-clients.sh — regenerate Go, TypeScript, and Python consumer client SDKs
# from clients/openapi.json. Idempotent: a second run produces zero git diff.
# Requirements: python3 (stdlib only), go 1.22+.
# The generator logic lives in scripts/gen_clients.py; this script is the entry point.
#
# clients/openapi.json is itself a generated artifact: `go run
# ./gateway/cmd/spec-dump` renders the gateway's real served OpenAPI document
# (openapi.Build + openapi.Handler); .github/workflows/client-sdk-drift.yml
# fails the build if the committed snapshot differs from that command's
# output, so this generator's input can never silently drift from the
# gateway's actual /openapi.json.
#
# Supports GET/POST/DELETE/PATCH operations, {param} path segments, and
# query parameters (typed as string or integer). Skips PUT/HEAD/OPTIONS/TRACE
# and billing-tagged routes (see notes on stderr).
#
# Drift check (run locally or in CI to assert no hand-edits have diverged):
#   bash scripts/gen-clients.sh && git diff --exit-code \
#     clients/go/client.go clients/go/errors.go \
#     clients/typescript/src/client.ts clients/typescript/src/errors.ts \
#     clients/typescript/src/index.ts \
#     clients/python/crucible_client/client.py clients/python/crucible_client/errors.py \
#     clients/python/crucible_client/__init__.py clients/python/tests/test_client.py
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
SPEC="${REPO_ROOT}/clients/openapi.json"

if [[ ! -f "${SPEC}" ]]; then
  echo "error: spec not found at ${SPEC}" >&2
  exit 1
fi

python3 "${SCRIPT_DIR}/gen_clients.py" "${SPEC}" "${REPO_ROOT}"

# Struct field alignment (json tag column widths) depends on the full set of
# field names/types in a struct, which this generator emits without tracking
# gofmt's alignment rules itself; gofmt is authoritative and already required
# (go 1.22+, see header), so let it do the alignment instead of duplicating
# its logic here. Scoped to exactly the files this generator writes — the
# hand-maintained webhook.go/webhook_test.go/webhook_internal_test.go in the
# same directory are untouched.
gofmt -w \
  "${REPO_ROOT}/clients/go/client.go" \
  "${REPO_ROOT}/clients/go/client_test.go" \
  "${REPO_ROOT}/clients/go/errors.go"
