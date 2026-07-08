#!/usr/bin/env bash
# acceptance-run.sh — runtime acceptance bar for a cloned Crucible tree.
#
# Proves a cloned tree runs the frozen HTTP/JSON contract, not just that it
# compiles (that's scripts/smoke-new-tool.sh's job). Two phases, both against
# the worker referenced by workers/active — never a hardcoded stub:
#
#   1. Contract conformance: scripts/conformance-run.sh, run against the
#      language workers/active resolves to (read from the symlink target, so
#      pointing workers/active at a different stub or a rebuilt product worker
#      changes what this checks without editing this script).
#   2. End-to-end billing: builds and starts the real workers/active binary,
#      then runs the Go test in gateway/test/acceptance against it through the
#      real gateway middleware chain (gateway/test/harness) and real
#      Postgres + Redis — asserting a metered /v1/<op> request is authed,
#      forwarded, billed (billable_units>=1), and produces exactly one
#      usage_events row.
#
# Usage:
#   POSTGRES_DSN=postgres://... REDIS_URL=redis://... bash scripts/acceptance-run.sh
#
# Requires: real Postgres + Redis reachable at POSTGRES_DSN / REDIS_URL.
# Migrations are applied automatically by the gateway test harness (schema
# boot behavior mirrors the production gateway; see gateway/test/harness).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ACTIVE_LINK="$REPO_ROOT/workers/active"

: "${POSTGRES_DSN:?POSTGRES_DSN must be set (real Postgres)}"
: "${REDIS_URL:?REDIS_URL must be set (real Redis)}"

if [[ ! -L "$ACTIVE_LINK" ]]; then
    echo "ERROR: $ACTIVE_LINK is not a symlink" >&2
    exit 1
fi

LINK_TARGET=$(readlink "$ACTIVE_LINK")
STUB_ID=$(basename "$LINK_TARGET")

case "$STUB_ID" in
    go|rust|ts|python) ;;
    *)
        echo "ERROR: workers/active -> $LINK_TARGET; unrecognised worker language '$STUB_ID' (expected go|rust|ts|python)" >&2
        exit 1
        ;;
esac

echo "==> workers/active -> $LINK_TARGET (language: $STUB_ID)"
echo

echo "=== Phase 1/2: contract conformance (workers/active) ==="
bash "$REPO_ROOT/scripts/conformance-run.sh" "$STUB_ID"
echo

echo "=== Phase 2/2: end-to-end billing acceptance (workers/active) ==="

WORKER_PID=""
WORKER_BIN=""

cleanup() {
    if [[ -n "$WORKER_PID" ]]; then
        kill "$WORKER_PID" 2>/dev/null || true
        wait "$WORKER_PID" 2>/dev/null || true
    fi
    [[ -n "$WORKER_BIN" ]] && rm -f "$WORKER_BIN" || true
}
trap cleanup EXIT INT TERM

# Grab a free port from the OS.
# Note: there is a TOCTOU window between selecting the port and the worker binding it.
# The early-exit check in the readiness loop (kill -0) catches the failure fast if it occurs.
PORT=$(python3 -c "import socket; s=socket.socket(); s.bind(('',0)); p=s.getsockname()[1]; s.close(); print(p)")

case "$STUB_ID" in
    go)
        WORKER_BIN="$(mktemp -t acceptance-worker.XXXXXX)"
        echo "==> Building workers/active (go)..."
        # cd -P resolves the workers/active symlink to its physical path before building.
        # go.work's `use` directives list physical module paths (e.g. ./workers/stubs/go);
        # building from the logical "workers/active" path makes the go tool think the
        # module isn't a workspace member ("not one of the workspace modules listed in go.work").
        (cd -P "$ACTIVE_LINK" && go build -o "$WORKER_BIN" .)
        echo "==> Starting workers/active on port $PORT..."
        PORT="$PORT" "$WORKER_BIN" &
        WORKER_PID=$!
        ;;
    rust)
        echo "==> Building workers/active (rust)..."
        (cd "$ACTIVE_LINK" && cargo build --release)
        RUST_BIN_NAME=$(sed -nE 's/^name *= *"(.*)"/\1/p' "$ACTIVE_LINK/Cargo.toml" | head -1)
        if [[ -z "$RUST_BIN_NAME" ]]; then
            echo "ERROR: could not read package name from $ACTIVE_LINK/Cargo.toml" >&2
            exit 1
        fi
        echo "==> Starting workers/active on port $PORT..."
        PORT="$PORT" "$ACTIVE_LINK/target/release/$RUST_BIN_NAME" &
        WORKER_PID=$!
        ;;
    ts)
        echo "==> Building TS SDK..."
        (cd "$REPO_ROOT/workers/sdk-ts" && npm ci && npm run build)
        echo "==> Building workers/active (ts)..."
        (cd "$ACTIVE_LINK" && npm ci && npm run build)
        echo "==> Starting workers/active on port $PORT..."
        PORT="$PORT" node "$ACTIVE_LINK/dist/index.js" &
        WORKER_PID=$!
        ;;
    python)
        echo "==> Starting workers/active (python) on port $PORT..."
        PORT="$PORT" python3 "$ACTIVE_LINK/worker.py" &
        WORKER_PID=$!
        ;;
esac

WORKER_URL="http://127.0.0.1:$PORT"
export WORKER_URL

# Wait for /healthz with a 30-second bounded timeout (300 x 0.1 s).
# Exits immediately if the worker process dies before becoming ready.
echo "==> Waiting for worker at $WORKER_URL/healthz ..."
for ((i = 0; i < 300; i++)); do
    if curl -sf "$WORKER_URL/healthz" >/dev/null 2>&1; then
        echo "==> Worker ready."
        break
    fi
    if ! kill -0 "$WORKER_PID" 2>/dev/null; then
        echo "ERROR: worker process exited before becoming ready" >&2
        exit 1
    fi
    sleep 0.1
done
if ! curl -sf "$WORKER_URL/healthz" >/dev/null 2>&1; then
    echo "ERROR: worker did not become ready within 30s" >&2
    exit 1
fi

echo "==> Running gateway acceptance test against $WORKER_URL ..."
cd "$REPO_ROOT/gateway"
go test -race -count=1 -v ./test/acceptance/...

echo
echo "acceptance-run: PASS"
