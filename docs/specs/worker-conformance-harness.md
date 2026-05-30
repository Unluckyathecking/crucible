# Spec: worker-conformance-harness

**Work unit:** `worker-conformance-harness`
**Branch:** `ultra-plan/20260529-worker-conformance-harness`
**Date:** 2026-05-29

## Scope

```
test/conformance/**
scripts/conformance*.sh
.github/workflows/worker-conformance.yml
```

A new, language-agnostic harness that boots a worker stub and asserts it satisfies the
frozen `/invoke` + `/healthz` contract (`gateway/proto/tool.proto`). It must run against
the existing Go stub (`workers/stubs/go/`) today and be structured so Rust and TypeScript
stubs can be added to the matrix later without changing the assertions.

## Input

A built, runnable worker stub binary/process exposing `POST /invoke` and `GET /healthz`
on a configurable port (discovered or passed via env, e.g. `WORKER_CMD` / `WORKER_PORT`).
The harness starts the worker (Go stub by default), waits for healthz, runs the suite,
and tears it down.

## Output

A reusable conformance test suite plus a CI job that fails when a worker violates the
contract. The suite is the single source of truth for "what a Crucible worker must do",
shared across every language SDK and every clone.

## Acceptance criteria

1. The harness boots the Go stub on an ephemeral/dynamic port and reaches a ready state without hanging (bounded startup timeout); all resources are cleaned up on every exit path.
2. Asserts `GET /healthz` returns HTTP 200 with body exactly `{"status":"ok"}`.
3. Asserts `POST /invoke` with a valid envelope returns HTTP 200 and a response that is either a success (`payload` present, `billable_units >= 1`) or a structured error (`error: {code, message, retryable}`) — never both, and never `billable_units < 1` on success.
4. Asserts the `billable_units` default: a success response with no/zero units is normalized to `>= 1` (mirrors the SDK + gateway trust boundary in `routes.go`).
5. Asserts a handler error yields the structured envelope `{"error":{"code":string,"message":string,"retryable":bool}}`.
6. A new CI workflow `.github/workflows/worker-conformance.yml` runs the harness against the Go stub and is green; it is structured (matrix or parametrized step) so additional stubs plug in by adding an entry, not by editing assertions.

## Forbidden

- No changes to `workers/sdk-ts/**` or `workers/stubs/ts/**` (in-flight PR #56).
- No changes to `gateway/proto/tool.proto` (frozen contract) or `gateway/internal/**` (the gateway already enforces the trust boundary; the harness only observes worker behavior).
- No changes to `workers/sdk-go/**` or `workers/stubs/go/**` — these are read-only reference subjects under test.
- No new always-on runtime dependencies for the gateway or workers; harness-only test deps are fine.
- Do not assert on fields outside the frozen contract (keep the suite portable across SDKs).

## Rationale

Crucible's whole value is a frozen worker contract that is identical across every clone and
every language SDK (Go shipped, Rust in review #6, TS in-flight #56), yet nothing boots a
worker and asserts the contract at runtime — `smoke-new-tool.sh` only checks compile-time
renames, and the gateway tests exercise the gateway, not workers. A shared conformance
harness is reusable infrastructure that locks the load-bearing invariant for all current and
future SDKs and is parallel-safe with #56 (disjoint scope; tests the Go stub on `main`).
