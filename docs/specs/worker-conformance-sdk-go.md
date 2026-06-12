# Directive: importable in-process Go worker-conformance harness (worker-conformance-sdk-go)

**Date:** 2026-06-12
**Type:** 10xworker:job (primary decomposition — workers / shared SDK infrastructure)
**Module:** `worker-conformance-sdk-go`

See the PR body for the full JSON spec, acceptance criteria, and forbidden list.

## Why

Crucible's clone-and-adapt promise rests on one frozen artifact: the
`gateway/proto/tool.proto` HTTP/JSON worker contract (billable_units >= 1 on success,
the `{error:{code,message,retryable}}` envelope, never-both, `/healthz` = `{"status":"ok"}`).
Today that contract is enforced two ways: the gateway's trust-boundary check in
`server/routes.go`, and the **external black-box** suite `test/conformance/contract_test.go`
(a standalone module that boots a worker as a subprocess over HTTP). What a product
developer lacks is an **importable, in-process** harness they can call from their own
worker's `go test` to self-verify a handler at dev time — the exact analog of wayline's
`wayline_adapter_registry::conformance::run(&adapter)` (#66), which made every vendor
adapter self-checkable in-process.

This unit adds that harness as a new subpackage of the shared SDK
(`workers/sdk-go/conformance/`): a product worker does
`conformance.Harness(t, handler)` and gets the full frozen-contract assertion set in
milliseconds, no subprocess, no port, no CI round-trip. It is the in-process sibling of
`test/conformance/contract_test.go`, NOT a replacement — it asserts the **same** contract
and must never weaken it. Extending `workers/sdk-go/` (not forking it) honors invariant #9,
and the leverage compounds: every Go-worker clone gains a one-line contract self-test, and
the Rust/TS/Python SDKs can follow the same shape later.

## Enabling seam

The contract wiring (`/healthz` + `/invoke`) lives today in the **unexported**
`invokeHandler`/`healthHandler` inside `crucible.go`, reachable only through `Serve(port, h)`
which binds a real port. To drive it in-process from a sibling package, factor out one
**additive, behavior-preserving** exported constructor:

```go
// Handler returns the worker's HTTP handler (/healthz + /invoke) without binding a port.
func Handler(h HandlerFunc) http.Handler { ... }
```

`Serve` is refactored to `http.ListenAndServe(addr, Handler(h))` — its observable behavior
and `invokeHandler`'s normalization/error logic are unchanged. The new `conformance` package
then drives `crucible.Handler(h)` via `httptest`.

## Scope (additive)

- `workers/sdk-go/conformance/**` (new package: harness + its own unit tests)
- `workers/sdk-go/crucible.go` (ADD the exported `Handler()` constructor ONLY; `Serve`
  delegates to it; no change to `invokeHandler`/`healthHandler` logic)

No edits to `gateway/**`, `gateway/proto/tool.proto`, or `test/conformance/**`.
