#!/usr/bin/env bash
# acceptance-run.sh — runtime acceptance bar for a cloned Crucible tree.
#
# Proves a cloned tree runs the frozen HTTP/JSON contract, not just that it
# compiles (that's scripts/smoke-new-tool.sh's job). Two phases, both against
# the SAME already-built, already-running workers/active binary — never a
# hardcoded stub, and never workers/stubs/<lang> standing in for it:
#
#   1. Contract conformance: the language-agnostic suite in test/conformance,
#      run against workers/active.
#   2. End-to-end billing: the Go test in gateway/test/acceptance, run
#      through the real gateway middleware chain (gateway/test/harness) and
#      real Postgres + Redis — asserting a metered /v1/<op> request is
#      authed, forwarded, billed (billable_units>=1), and produces exactly
#      one usage_events row.
#
# workers/active's language is detected from files inside it (go.mod,
# Cargo.toml, package.json, worker.py), not from the symlink target's
# basename — ADAPT.md sanctions repointing workers/active at a brand new
# workers/<product>/ directory, which need not be named after a language.
# scripts/conformance-run.sh is deliberately NOT invoked here: it always
# builds workers/stubs/<lang>, which is a different tree from workers/active
# for any clone that adapted into its own directory.
#
# Usage:
#   POSTGRES_DSN=postgres://... REDIS_URL=redis://... bash scripts/acceptance-run.sh
#
# Requires: real Postgres + Redis reachable at POSTGRES_DSN / REDIS_URL.
# Migrations are applied automatically by the gateway test harness (schema
# boot behavior mirrors the production gateway; see gateway/test/harness).
# WORKER_SHARED_SECRET, if set, is passed through to both the worker process
# (the SDKs read it automatically) and the gateway acceptance test.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ACTIVE_LINK="$REPO_ROOT/workers/active"

: "${POSTGRES_DSN:?POSTGRES_DSN must be set (real Postgres)}"
: "${REDIS_URL:?REDIS_URL must be set (real Redis)}"

if [[ ! -L "$ACTIVE_LINK" ]]; then
    echo "ERROR: $ACTIVE_LINK is not a symlink" >&2
    exit 1
fi

# cd -P resolves the workers/active symlink to its physical path. go.work's
# `use` directives list physical module paths (e.g. ./workers/stubs/go);
# building from the logical "workers/active" path makes the go tool think the
# module isn't a workspace member ("not one of the workspace modules listed
# in go.work"). All downstream steps operate on this resolved path.
ACTIVE_DIR="$(cd -P "$ACTIVE_LINK" && pwd)"

detect_worker_lang() {
    local dir="$1"
    if [[ -f "$dir/go.mod" ]]; then
        echo go
    elif [[ -f "$dir/Cargo.toml" ]]; then
        echo rust
    elif [[ -f "$dir/package.json" ]]; then
        echo ts
    elif [[ -f "$dir/worker.py" ]]; then
        echo python
    fi
}

LANG_ID=$(detect_worker_lang "$ACTIVE_DIR")
if [[ -z "$LANG_ID" ]]; then
    echo "ERROR: could not determine the language of workers/active ($ACTIVE_DIR) — expected a go.mod, Cargo.toml, package.json, or worker.py" >&2
    exit 1
fi

echo "==> workers/active -> $ACTIVE_DIR (language: $LANG_ID)"
echo

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

case "$LANG_ID" in
    go)
        WORKER_BIN="$(mktemp -t acceptance-worker.XXXXXX)"
        echo "==> Building workers/active (go)..."
        (cd "$ACTIVE_DIR" && go build -o "$WORKER_BIN" .)
        echo "==> Starting workers/active on port $PORT..."
        PORT="$PORT" "$WORKER_BIN" &
        WORKER_PID=$!
        ;;
    rust)
        echo "==> Building workers/active (rust)..."
        (cd "$ACTIVE_DIR" && cargo build --release)
        RUST_BIN_NAME=$(sed -nE 's/^name *= *"(.*)"/\1/p' "$ACTIVE_DIR/Cargo.toml" | head -1)
        if [[ -z "$RUST_BIN_NAME" ]]; then
            echo "ERROR: could not read package name from $ACTIVE_DIR/Cargo.toml" >&2
            exit 1
        fi
        echo "==> Starting workers/active on port $PORT..."
        PORT="$PORT" "$ACTIVE_DIR/target/release/$RUST_BIN_NAME" &
        WORKER_PID=$!
        ;;
    ts)
        echo "==> Building TS SDK..."
        (cd "$REPO_ROOT/workers/sdk-ts" && npm ci && npm run build)
        echo "==> Building workers/active (ts)..."
        (cd "$ACTIVE_DIR" && npm ci && npm run build)
        echo "==> Starting workers/active on port $PORT..."
        PORT="$PORT" node "$ACTIVE_DIR/dist/index.js" &
        WORKER_PID=$!
        ;;
    python)
        echo "==> Starting workers/active (python) on port $PORT..."
        PORT="$PORT" python3 "$ACTIVE_DIR/worker.py" &
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
echo

echo "=== Phase 1/2: contract conformance (workers/active) ==="
(cd "$REPO_ROOT/test/conformance" && GOWORK=off go test -v ./...)
echo

echo "=== Phase 2/2: end-to-end billing acceptance (workers/active) ==="
cd "$REPO_ROOT/gateway"
go test -race -count=1 -v ./test/acceptance/...

echo
echo "acceptance-run: PASS"
