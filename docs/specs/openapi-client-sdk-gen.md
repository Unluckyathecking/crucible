# 10xworker:job — `openapi-client-sdk-gen`

> Primary decomposition spec. The canonical machine-readable contract is the JSON block in the PR body; this document is the human-facing decomposition for downstream 10X workers.

## Why this phase

Crucible advertises a clone-and-adapt framework with "OpenAPI/SDK generation" as a first-class feature. Today `gateway/internal/openapi` builds a static OpenAPI 3.1 document and serves it at `GET /openapi.json`, but there is **no generated consumer client SDK** — only the worker-side SDKs (`workers/sdk-go`, `workers/sdk-rust`, `workers/stubs/*`). Every team integrating *against* a Crucible-built API must hand-roll an HTTP client and re-derive the request/response and error-envelope shapes. This phase closes that gap with reusable, regenerable client SDKs and a CI drift guard, so each clone ships type-safe clients for free and every future endpoint propagates into the SDKs on regeneration.

This is **consumer/client SDK** work and is deliberately disjoint from the worker-side TypeScript SDK in open PR #56 (`workers/sdk-ts/**`). Different module boundary, different audience, no shared files.

## Module boundary

- New top-level `clients/` tree: `clients/go/` (standalone Go module) and `clients/typescript/` (standalone npm package).
- New `scripts/gen-clients.sh` regeneration entrypoint.
- New `.github/workflows/client-sdk-drift.yml` CI job (NOT `ci.yml`, which open PR #88 owns).
- `gateway/internal/openapi/openapi.go` is **read-only input**.

## Suggested decomposition for downstream workers

1. **Spec source** — read the OpenAPI doc either by invoking the `openapi` package's builder from a tiny Go `//go:generate`-style helper, or from a committed `clients/openapi.json` snapshot that the script regenerates. The script must be deterministic and runnable in CI without a live gateway.
2. **Go client** (`clients/go/`) — its own `go.mod`; a `Client` with a constructor taking a base URL and a caller-supplied `*http.Client` (so timeouts are the caller's choice — do **not** hardcode); typed request/response structs and a typed error mapping the `{error:{code,message,retryable}}` envelope to a Go error.
3. **TypeScript client** (`clients/typescript/`) — strict TS, `fetch`-based, no runtime deps beyond the runtime's `fetch`; typed request/response interfaces and a typed error/exception for the envelope.
4. **Drift guard** — `client-sdk-drift.yml` runs `scripts/gen-clients.sh` then `git diff --exit-code clients/`, failing CI if the committed clients are stale relative to the spec.

## Acceptance (verifiable from the diff)

See the PR-body JSON `acceptance` array. In summary: `scripts/gen-clients.sh` is idempotent; `cd clients/go && go build ./... && go test -race ./...` is green; `cd clients/typescript && npm ci && npm run build && npm test` is green with `tsc --strict --noEmit`; both clients type the error envelope; the drift workflow fails on un-regenerated spec changes; nothing under `gateway/**`, `workers/**`, or `dashboard/**` is modified.

## Forbidden

- No changes to `gateway/proto/tool.proto` or any `gateway/internal/**` package.
- No changes to `workers/sdk-ts/**` or `workers/stubs/ts/**` (open PR #56's boundary).
- No runtime SDK generation inside the gateway binary — build/CI-time only.
- No edits to `.github/workflows/ci.yml` (open PR #88) — add a new workflow.
- No new runtime dependency in the gateway Go module or the dashboard.

## Scope LOC

Estimated well under the 10k cap (small API surface → small generated clients + script + workflow + spec).
