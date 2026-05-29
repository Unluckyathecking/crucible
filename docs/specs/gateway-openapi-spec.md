# Spec: gateway-openapi-spec

**Work unit:** `gateway-openapi-spec`
**Branch:** `ultra-plan/20260529-gateway-openapi-spec`
**Date:** 2026-05-29

## Context

The README lists "OpenAPI/SDK generation" as a core responsibility the gateway
"owns", but no implementation exists — `gateway/internal/server/` contains only
`routes.go` + `routes_test.go`, and there is no `openapi` package anywhere in the
repo. For a clone-and-adapt framework this is the single highest-leverage missing
capability: a served, machine-readable API description is what lets every clone
generate typed client SDKs and publish docs for free.

This unit is **parallel-safe** with the in-flight worker-SDK PR #56: that PR is
confined to `workers/**` and explicitly forbids gateway changes; this unit is
confined to the gateway and forbids worker changes.

## Scope

```
gateway/internal/openapi/**
gateway/internal/server/routes.go   # additive route mount ONLY
```

## Input

The gateway's existing public route table (auth/key, invoke, healthz, metrics),
the frozen worker contract (`gateway/proto/tool.proto`, read-only), and the
shared error envelope already emitted by handlers (`{error:{code,message,retryable}}`).

## Output

- A new `gateway/internal/openapi` package that builds an **OpenAPI 3.1**
  document for the gateway's public surface using plain Go structs +
  `encoding/json` (zero new third-party dependency — matches the zero-dep SDK
  ethos).
- A served read-only endpoint `GET /openapi.json` returning the document.

## Acceptance criteria

1. `cd gateway && go vet ./... && go test -race ./...` passes.
2. `GET /openapi.json` returns `200` with `Content-Type: application/json`; the
   decoded body has `openapi == "3.1.0"` and a non-empty `paths` object that
   includes at least the invoke route and `/healthz`.
3. An `apiKey`-type security scheme (HTTP header) is declared under
   `components.securitySchemes` and referenced by the protected operation(s).
4. The error envelope is declared once as a reusable `components.schemas` entry
   and `$ref`-d by the relevant responses (no inline duplication).
5. A unit test in `gateway/internal/openapi` asserts the generated document
   contains the required paths, the security scheme, and the error component
   (golden-file or structural assertions both acceptable).
6. Serving `/openapi.json` requires neither Postgres nor Redis (document is
   derived/static), so it works in the smoke environment.

## Forbidden

- No changes to `gateway/proto/tool.proto` (frozen contract).
- No changes to `workers/**` (keeps this parallel-safe with open PR #56).
- No new third-party module dependency — generate with stdlib `encoding/json`;
  do **not** add `kin-openapi`, `swaggo`, or an openapi-generator build step.
- In `routes.go`, only **add** the `/openapi.json` route mount; do not modify the
  `invoke`, auth, billing, ratelimit, quota, or proxy handler behaviour.
- The endpoint must be unauthenticated and side-effect free (no DB/Redis writes).

## Rationale

OpenAPI generation is the biggest framework capability the gateway advertises but
does not yet implement; delivering it as a reusable, dependency-free document
builder compounds across every Crucible clone and unblocks downstream typed-SDK
generation. Scope is sharply bounded to a new package plus one additive route, so
it composes cleanly alongside the worker-SDK lane (#56) and the open gateway
hardening PRs.
