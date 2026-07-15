#!/usr/bin/env bash
# acceptance-run.sh — runtime acceptance bar for a cloned Crucible tree.
#
# Proves a cloned tree runs the frozen HTTP/JSON contract, not just that it
# compiles (that's scripts/smoke-new-tool.sh's job). Two phases, both against
# the SAME already-built workers/active binary — never a hardcoded stub, and
# never workers/stubs/<lang> standing in for it:
#
#   1. Contract conformance: the language-agnostic suite in test/conformance,
#      run against an unsigned workers/active instance (the conformance
#      fixtures don't sign requests, so channel-auth is never enabled here).
#   2. End-to-end billing: the Go test in gateway/test/acceptance, run
#      through the real gateway middleware chain (gateway/test/harness) and
#      real Postgres + Redis against a workers/active instance signed with
#      WORKER_SHARED_SECRET when set — asserting a metered /v1/<op> request
#      is authed, forwarded, billed (billable_units>=1), and produces
#      exactly one usage_events row.
#
# workers/active's language is detected from files inside it (go.mod,
# Cargo.toml, package.json, worker.py), not from the symlink target's
# basename — ADAPT.md sanctions repointing workers/active at a brand new
# workers/<product>/ directory, which need not be named after a language.
# scripts/conformance-run.sh is deliberately NOT invoked here: it always
# builds workers/stubs/<lang>, which is a different tree from workers/active
# for any clone that adapted into its own directory.
#
# Known limitation (owned by test/conformance, out of this script's scope to
# fix): the conformance suite's fixtures always send operation:"echo", so a
# worker that rejects unknown operations will fail phase 1 even if it
# correctly implements its own product operation.
#
# Phase 2 is no longer limited to an empty JSON object: gateway/test/acceptance
# sends server.V1Routes[0].SampleRequest when the clone has declared one
# (RouteDescriptor.SampleRequest, gateway/internal/openapi), falling back to {}
# only when the route has no sample — so a route whose worker requires
# specific payload fields can be proven end-to-end by declaring a sample in
# routes_table.go.
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

# cd -P resolves the workers/active symlink to its physical path. All
# downstream steps operate on this resolved path.
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

WORKER_BIN=""
RUST_BIN_NAME=""
TS_ENTRYPOINT=""

case "$LANG_ID" in
    go)
        WORKER_BIN="$(mktemp -t acceptance-worker.XXXXXX)"
        echo "==> Building workers/active (go)..."
        # GOWORK=off builds workers/active using only its own go.mod (and any
        # replace directives it declares), independent of the repo-root
        # go.work `use` list. Without this, a product that repoints
        # workers/active at a brand new workers/<product>/ directory (per
        # ADAPT.md) fails to build unless that directory is also added to
        # go.work — an extra step this script must not require.
        # A preceding `go work sync` can raise an indirect dependency selected
        # by the active module without populating that module's go.sum. The
        # acceptance workflow intentionally builds with GOWORK=off, so fetch
        # the synchronized module graph before invoking the read-only build.
        (cd "$ACTIVE_DIR" && GOWORK=off go mod download && GOWORK=off go build -o "$WORKER_BIN" .)
        ;;
    rust)
        echo "==> Building workers/active (rust)..."
        (cd "$ACTIVE_DIR" && cargo build --release)
        RUST_BIN_NAME=$(sed -nE 's/^name *= *"(.*)"/\1/p' "$ACTIVE_DIR/Cargo.toml" | head -1)
        if [[ -z "$RUST_BIN_NAME" ]]; then
            echo "ERROR: could not read package name from $ACTIVE_DIR/Cargo.toml" >&2
            exit 1
        fi
        ;;
    ts)
        echo "==> Building TS SDK..."
        (cd "$REPO_ROOT/workers/sdk-ts" && npm ci && npm run build)
        echo "==> Building workers/active (ts)..."
        (cd "$ACTIVE_DIR" && npm ci && npm run build)
        # Resolve the package's own declared entrypoint (package.json "main",
        # defaulting to the stub's dist/index.js layout only as a fallback)
        # instead of assuming every product keeps that exact build output path.
        TS_ENTRYPOINT=$(node -e "console.log(require('$ACTIVE_DIR/package.json').main || 'dist/index.js')")
        if [[ ! -f "$ACTIVE_DIR/$TS_ENTRYPOINT" ]]; then
            echo "ERROR: workers/active package.json \"main\" ($TS_ENTRYPOINT) does not exist after build" >&2
            exit 1
        fi
        ;;
    python)
        # No build step; worker.py is run directly.
        ;;
esac
echo

WORKER_PID=""
WORKER_URL=""

# start_worker launches the already-built workers/active binary on a fresh
# port, with secret (if non-empty) as its WORKER_SHARED_SECRET so the SDK's
# automatic channel-auth enforcement matches what the caller passes to
# stop_worker's counterpart on the client side. Sets WORKER_PID/WORKER_URL.
start_worker() {
    local secret="$1"
    local port
    # Grab a free port from the OS.
    # Note: there is a TOCTOU window between selecting the port and the worker
    # binding it. The early-exit check in the readiness loop (kill -0) below
    # catches the failure fast if it occurs.
    port=$(python3 -c "import socket; s=socket.socket(); s.bind(('',0)); p=s.getsockname()[1]; s.close(); print(p)")

    case "$LANG_ID" in
        go)
            echo "==> Starting workers/active on port $port..."
            PORT="$port" WORKER_SHARED_SECRET="$secret" "$WORKER_BIN" &
            WORKER_PID=$!
            ;;
        rust)
            echo "==> Starting workers/active on port $port..."
            PORT="$port" WORKER_SHARED_SECRET="$secret" "$ACTIVE_DIR/target/release/$RUST_BIN_NAME" &
            WORKER_PID=$!
            ;;
        ts)
            echo "==> Starting workers/active on port $port..."
            # Runs the entrypoint resolved from package.json "main" (above)
            # directly, not `npm start` in a subshell — killing WORKER_PID
            # must reach the actual node process, not an intermediate shell.
            PORT="$port" WORKER_SHARED_SECRET="$secret" node "$ACTIVE_DIR/$TS_ENTRYPOINT" &
            WORKER_PID=$!
            ;;
        python)
            echo "==> Starting workers/active on port $port..."
            PORT="$port" WORKER_SHARED_SECRET="$secret" python3 "$ACTIVE_DIR/worker.py" &
            WORKER_PID=$!
            ;;
    esac

    WORKER_URL="http://127.0.0.1:$port"

    # Wait for /healthz with a 30-second bounded timeout (300 x 0.1 s).
    # Exits immediately if the worker process dies before becoming ready.
    echo "==> Waiting for worker at $WORKER_URL/healthz ..."
    local i
    for ((i = 0; i < 300; i++)); do
        if curl -sf "$WORKER_URL/healthz" >/dev/null 2>&1; then
            echo "==> Worker ready."
            return 0
        fi
        if ! kill -0 "$WORKER_PID" 2>/dev/null; then
            echo "ERROR: worker process exited before becoming ready" >&2
            exit 1
        fi
        sleep 0.1
    done
    echo "ERROR: worker did not become ready within 30s" >&2
    exit 1
}

stop_worker() {
    if [[ -n "$WORKER_PID" ]]; then
        kill "$WORKER_PID" 2>/dev/null || true
        wait "$WORKER_PID" 2>/dev/null || true
        WORKER_PID=""
    fi
}

cleanup() {
    stop_worker
    [[ -n "$WORKER_BIN" ]] && rm -f "$WORKER_BIN" || true
}
trap cleanup EXIT INT TERM

echo "=== Phase 1/2: contract conformance (workers/active, unsigned) ==="
# test/conformance's fixtures don't sign requests, so this instance always
# runs with channel-auth off regardless of WORKER_SHARED_SECRET.
start_worker ""
export WORKER_URL
(cd "$REPO_ROOT/test/conformance" && GOWORK=off go test -v ./...)
stop_worker
echo

echo "=== Phase 2/2: end-to-end billing acceptance (workers/active) ==="
start_worker "${WORKER_SHARED_SECRET:-}"
export WORKER_URL
export WORKER_SHARED_SECRET="${WORKER_SHARED_SECRET:-}"
cd "$REPO_ROOT/gateway"
go test -race -count=1 -v ./test/acceptance/...

echo
echo "acceptance-run: PASS"
