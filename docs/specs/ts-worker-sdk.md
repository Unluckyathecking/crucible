# Spec: ts-worker-sdk

**Work unit:** `ts-worker-sdk`  
**Branch:** `ultra-plan/20260528-ts-worker-sdk`  
**Date:** 2026-05-28

## Scope

```
workers/sdk-ts/**
workers/stubs/ts/**
```

## Input

An HTTP POST to `/invoke` carrying `{request_id, customer_id, operation, payload, plan, metadata}` per `gateway/proto/tool.proto`.

## Output

A runnable Node.js/TypeScript HTTP server that satisfies the frozen worker contract, with a hello-world stub and a zero-dependency SDK package.

## Acceptance criteria

1. `npm test` in `workers/sdk-ts/` passes with TypeScript strict (`tsc --strict --noEmit` exits 0).
2. `workers/stubs/ts/` serves `POST /invoke` returning `{payload, billable_units, units_label}` and `GET /healthz` → 200.
3. `make worker` works with the TS stub (or a `TS_WORKER=1` variant is documented with exact command).
4. SDK package exports a type-safe `WorkerHandler<TInput, TOutput>` interface matching the proto contract.
5. No gateway files modified; `gateway/proto/tool.proto` is untouched.
6. The hello-world stub mirrors the Go stub behavior: `operation: "echo"` returns `{echo: payload, operation}` with `billable_units: metadata.units ?? 1`.

## Forbidden

- No changes to `gateway/proto/tool.proto`.
- No changes to `gateway/internal/` packages.
- No changes to `workers/sdk-go/` (shared Go SDK).
- No runtime dependencies beyond Node.js stdlib equivalents; zero-dep philosophy matches the Go and Rust SDKs.
- No changes to `workers/active` symlink (worker decides which to run; documentation only).

## Rationale

Crucible advertises polyglot workers ("Workers can be written in any language that speaks HTTP/JSON") but ships only a Go SDK and a pending Rust SDK. TypeScript/JavaScript is the most common runtime for new API products, and its absence forces JS-native teams to context-switch into Go or Rust just to write a 30-line product adapter. This module closes the polyglot gap and completes the v1 worker story alongside the existing Go and Rust SDKs.
