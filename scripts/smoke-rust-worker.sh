#!/usr/bin/env bash
set -euo pipefail

# Smoke test for the Rust example worker
# Starts the worker, sends an /invoke request, verifies the response shape.

WORKER_PORT="${WORKER_PORT:-18081}"
WORKER_URL="http://127.0.0.1:${WORKER_PORT}"

echo "=== Rust Worker Smoke Test ==="

# Build the example
cd workers/sdk-rust
cargo build --example hello 2>&1
cd ../..

# Start worker in background
WORKER_PORT="$WORKER_PORT" ./workers/sdk-rust/target/debug/examples/hello &
WORKER_PID=$!

# Cleanup on exit
cleanup() {
    echo "Cleaning up worker (PID $WORKER_PID)..."
    kill "$WORKER_PID" 2>/dev/null || true
    wait "$WORKER_PID" 2>/dev/null || true
}
trap cleanup EXIT

# Wait for healthz
for i in {1..30}; do
    if curl -sf "${WORKER_URL}/healthz" >/dev/null 2>&1; then
        echo "Worker ready"
        break
    fi
    sleep 0.5
done

# Verify healthz
HEALTH=$(curl -sf "${WORKER_URL}/healthz")
if [ "$HEALTH" != '{"status":"ok"}' ]; then
    echo "ERROR: unexpected healthz response: $HEALTH"
    exit 1
fi
echo "healthz: OK"

# Verify readyz
READY=$(curl -sf "${WORKER_URL}/readyz")
if [ "$READY" != '{"status":"ok"}' ]; then
    echo "ERROR: unexpected readyz response: $READY"
    exit 1
fi
echo "readyz: OK"

# Invoke
RESP=$(curl -sf "${WORKER_URL}/invoke" \
    -H "Content-Type: application/json" \
    -d '{"request_id":"smoke-1","customer_id":"cust-1","operation":"hello","payload":"\"world\"","plan":"pro","metadata":{}}')

echo "invoke response: $RESP"

# Verify response shape
if ! echo "$RESP" | grep -q '"payload"'; then
    echo "ERROR: response missing payload"
    exit 1
fi
if ! echo "$RESP" | grep -q '"billable_units"'; then
    echo "ERROR: response missing billable_units"
    exit 1
fi

# Verify billable_units defaults to 1
UNITS=$(echo "$RESP" | grep -o '"billable_units":[0-9]*' | cut -d: -f2)
if [ "$UNITS" != "1" ]; then
    echo "ERROR: expected billable_units=1, got $UNITS"
    exit 1
fi

echo "=== Rust Worker Smoke Test PASSED ==="
