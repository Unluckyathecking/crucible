#!/usr/bin/env bash
# smoke-new-tool.sh — verify that the clone-and-rename script produces a buildable tree.
#
# This is the LOAD-BEARING test of the framework's value proposition: if a developer
# can't clone and have something compile, the entire framework is dead. Run on every PR
# (via .github/workflows/new-tool-smoke.yml) AND locally (via `make smoke-test-new-tool`).
set -euo pipefail

# Portable in-place sed (macOS requires -i '', Linux uses -i alone).
_sed_i() {
  if [[ "$(uname)" == "Darwin" ]]; then
    sed -i '' "$@"
  else
    sed -i "$@"
  fi
}

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

echo "==> doctor: preflight adapt-drift checks (pass on correctly-adapted tree)"
bash scripts/doctor.sh

echo "==> doctor: inject env-prefix drift, assert non-zero exit + named cause"
cp dashboard/.env.example dashboard/.env.example.bak
_sed_i 's/^API_KEY_PREFIX=.*/API_KEY_PREFIX=drifted_/' dashboard/.env.example
doctor_rc=0
doctor_out=$(bash scripts/doctor.sh 2>&1) || doctor_rc=$?
mv dashboard/.env.example.bak dashboard/.env.example
if [[ $doctor_rc -eq 0 ]]; then
  echo "FAIL: doctor should exit non-zero on env-prefix drift but exited 0" >&2
  exit 1
fi
if [[ "$doctor_out" != *"env_prefix_mismatch"* ]]; then
  echo "FAIL: doctor output missing named cause 'env_prefix_mismatch'" >&2
  printf 'Got:\n%s\n' "$doctor_out" >&2
  exit 1
fi
echo "  -> doctor correctly detected env-prefix drift (exit $doctor_rc, cause: env_prefix_mismatch)"

echo "==> non-Go surfaces"

# --- Rust SDK -----------------------------------------------------------------
# Gate on cargo; machines without Rust toolchain skip this check gracefully.
if command -v cargo >/dev/null 2>&1; then
  echo "  -> cargo check workers/sdk-rust"
  (cd workers/sdk-rust && cargo check --quiet 2>&1)
else
  echo "  -> cargo not found — skipping Rust SDK check"
fi

# --- Python stub --------------------------------------------------------------
# Gate on python3; machines without Python skip this check gracefully.
if command -v python3 >/dev/null 2>&1; then
  echo "  -> py_compile workers/stubs/python/worker.py"
  python3 -c "import py_compile; py_compile.compile('workers/stubs/python/worker.py', doraise=True)"

  echo "  -> smoke-run: start worker, hit /healthz, stop worker"
  WORKER_PORT=19081
  PORT=$WORKER_PORT python3 workers/stubs/python/worker.py &
  WORKER_PID=$!
  # Wait up to 3 seconds for the worker to be ready.
  ok=0
  for _i in 1 2 3 4 5 6; do
    sleep 0.5
    if curl -sf "http://localhost:${WORKER_PORT}/healthz" >/dev/null 2>&1; then
      ok=1
      break
    fi
  done
  kill "$WORKER_PID" 2>/dev/null || true
  wait "$WORKER_PID" 2>/dev/null || true
  if [[ $ok -ne 1 ]]; then
    echo "FAIL: Python stub /healthz did not return 200" >&2
    exit 1
  fi
else
  echo "  -> python3 not found — skipping Python stub check"
fi

echo
echo "smoke-new-tool: PASS"
