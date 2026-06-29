# Work order — Cross-SDK conformance parity

Decomposition spec for the 10X worker. Grounded in `main` HEAD `7e8fc20`. Direction
context: `docs-internal/further-steps-2026-06-28.md` item 2. Extends PR #124.

This document is **direction, not implementation**. Build against the JSON spec in the PR
body. Test + CI only — no gateway and no SDK runtime-behaviour changes.

## Why this is the next phase

The frozen HTTP/JSON `/invoke` contract is the load-bearing promise of clone-and-adapt, and
three worker SDKs now implement it (Go, Rust, TS). PR #124 added the non-POST→405 assertion,
but the conformance assertions live in the **Go** harness (`workers/sdk-go/conformance/`) and
the CI matrix runs that one Go suite against every stub. There is **no shared, language-neutral
spec/fixture**, so the contract can drift per language — the TS SDK (newest) is the likeliest
to diverge. This phase builds a reusable conformance harness that compounds across every
future SDK and every clone.

## Module boundary (shared abstraction, not a one-off test)

A language-neutral conformance spec + fixture (`workers/conformance/`) that enumerates the
contract's checkable behaviours as data, plus a per-SDK runner that loads the same fixture and
asserts identical outcomes, plus the CI matrix wiring. The fixture is the composable artifact:
adding a new SDK or a new contract rule is a fixture/runner edit, not a per-language rewrite.

## Behaviours the matrix must assert identically across Go/Rust/TS

- `/invoke` non-POST → `405` (method guard).
- `billable_units >= 1` floor: a worker returning `0` is normalised to `1`.
- `apierror` error-envelope shape: `{error:{code,message,retryable}}` on handler error.
- `/healthz` exact body `{"status":"ok"}`.

(Today: Go harness asserts all four; Rust relies on the axum `post()` combinator and has no
dedicated conformance file; TS has ad-hoc method-guard/envelope tests with no shared fixture.)

## Suggested sub-units for the 10X worker (one coherent PR)

1. `workers/conformance/spec.md` + `workers/conformance/fixture.json` — the contract rules as
   data (method-guard cases, billable-units floor, envelope shape, healthz body).
2. Rust conformance runner under `workers/sdk-rust/conformance/` loading the shared fixture.
3. TS conformance runner under `workers/sdk-ts/conformance/` loading the shared fixture.
4. Go parity test under `workers/sdk-go/conformance/` that loads the same fixture (so all
   three read one source of truth), with one new failing-then-passing case added per SDK.
5. CI: extend `.github/workflows/worker-conformance.yml` so each SDK runs its fixture-driven
   runner; `fail-fast: false` preserved.

## Acceptance (verifiable from the diff)

- `workers/conformance/fixture.json` exists and is the single source of truth all three
  runners load (grep: each runner references the shared fixture path).
- A new conformance case is added that fails before and passes after, in each of Go/Rust/TS.
- `.github/workflows/worker-conformance.yml` runs a fixture-driven conformance step for
  go, rust, and ts; workflow green.
- No file under `gateway/**` is modified; no SDK runtime/behaviour file is modified (the diff
  is limited to `workers/*/conformance/**`, `workers/conformance/**`, and the workflow).

## Forbidden

- No changes under `gateway/**` (no auth/billing/proxy/ratelimit/quota edits).
- No changes to SDK runtime behaviour — conformance is test + CI only. If a runner reveals a
  real divergence, open a separate PR for the fix; this PR only adds the harness + a
  red→green case.
- No edits to `gateway/proto/tool.proto` (frozen).
- No edits under `scripts/**` (reserved for the parallel `new-tool.sh` doctor claim); put any
  runner orchestration under `workers/conformance/`.
