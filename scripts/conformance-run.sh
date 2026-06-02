#!/usr/bin/env bash
# conformance-run.sh <stub-id>
#
# Builds and starts a worker stub on a dynamic port, waits for readiness,
# runs the contract conformance suite, then cleans up on every exit path.
#
# Usage:
#   bash scripts/conformance-run.sh go
#
# Adding a new stub: add a case entry below and a matrix entry in
# .github/workflows/worker-conformance.yml — no changes to the test assertions needed.

set -euo pipefail

STUB="${1:-go}"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

STUB_PID=""
STUB_BIN=""

cleanup() {
    if [[ -n "$STUB_PID" ]]; then
        kill "$STUB_PID" 2>/dev/null || true
        wait "$STUB_PID" 2>/dev/null || true
    fi
    [[ -n "$STUB_BIN" ]] && rm -f "$STUB_BIN" || true
}
trap cleanup EXIT INT TERM

# Grab a free port from the OS.
# Note: there is a TOCTOU window between selecting the port and the worker binding it.
# The early-exit check in the readiness loop (kill -0) catches the failure fast if it occurs.
PORT=$(python3 -c "import socket; s=socket.socket(); s.bind(('',0)); p=s.getsockname()[1]; s.close(); print(p)")

case "$STUB" in
  go)
    STUB_BIN="$(mktemp -t conformance-worker.XXXXXX)"
    echo "==> Building Go stub..."
    (cd "$REPO_ROOT/workers/stubs/go" && go build -o "$STUB_BIN" .)
    echo "==> Starting Go stub on port $PORT..."
    PORT="$PORT" "$STUB_BIN" &
    STUB_PID=$!
    ;;
  rust)
    echo "==> Building Rust stub..."
    (cd "$REPO_ROOT/workers/stubs/rust" && cargo build --release)
    echo "==> Starting Rust stub on port $PORT..."
    PORT="$PORT" "$REPO_ROOT/workers/stubs/rust/target/release/crucible-stub-rust" &
    STUB_PID=$!
    ;;
  ts)
    echo "==> Building TS SDK..."
    (cd "$REPO_ROOT/workers/sdk-ts" && npm ci && npm run build)
    echo "==> Building TS stub..."
    (cd "$REPO_ROOT/workers/stubs/ts" && npm ci && npm run build)
    echo "==> Starting TS stub on port $PORT..."
    PORT="$PORT" node "$REPO_ROOT/workers/stubs/ts/dist/index.js" &
    STUB_PID=$!
    ;;
  python)
    echo "==> Starting Python stub on port $PORT..."
    PORT="$PORT" python3 "$REPO_ROOT/workers/stubs/python/worker.py" &
    STUB_PID=$!
    ;;
  *)
    echo "Unknown stub id: $STUB" >&2
    echo "Supported stubs: go|rust|ts|python" >&2
    exit 1
    ;;
esac

WORKER_URL="http://127.0.0.1:$PORT"
export WORKER_URL

# Wait for /healthz with a 30-second bounded timeout (300 × 0.1 s).
# Exits immediately if the stub process dies before becoming ready.
echo "==> Waiting for worker at $WORKER_URL/healthz ..."
for ((i=0; i<300; i++)); do
    if curl -sf "$WORKER_URL/healthz" >/dev/null 2>&1; then
        echo "==> Worker ready."
        break
    fi
    if ! kill -0 "$STUB_PID" 2>/dev/null; then
        echo "ERROR: worker process exited before becoming ready" >&2
        exit 1
    fi
    sleep 0.1
done
if ! curl -sf "$WORKER_URL/healthz" >/dev/null 2>&1; then
    echo "ERROR: worker did not become ready within 30s" >&2
    exit 1
fi

# Run the language-agnostic conformance suite.
echo "==> Running conformance suite against $WORKER_URL ..."
cd "$REPO_ROOT/test/conformance"
GOWORK=off go test -v ./...
