# `openapi-client-sdk-gen` — implementation reference

> This document describes the delivered implementation. The canonical machine-readable contract is the JSON block in PR #94.

## What was built

| Artifact | Path | Purpose |
|---|---|---|
| OpenAPI snapshot | `clients/openapi.json` | Canonical source-of-truth for generation; exact output of `gateway/internal/openapi.Build()` |
| Generator script | `scripts/gen-clients.sh` | Reads the snapshot; uses embedded Python 3 (stdlib only) to regenerate all three SDKs deterministically |
| Go client | `clients/go/` | Standalone `go.mod` module; typed `Client`, typed `APIError`, zero external deps |
| TypeScript client | `clients/typescript/` | Strict-TS npm package; typed `Client`, typed `ApiError`, no runtime deps beyond `fetch` |
| Python client | `clients/python/` | Pip-installable `crucible_client` package (named to avoid colliding with the `crucible` package the worker SDK already ships); typed `Client` (TypedDict responses), typed `ApiError`, zero runtime deps beyond `urllib`/`json` (stdlib) |
| Drift CI | `.github/workflows/client-sdk-drift.yml` | Runs generator then `git diff --exit-code clients/`; fails if committed clients are stale |

## Design decisions

### Snapshot instead of live gateway

`scripts/gen-clients.sh` reads `clients/openapi.json` — a committed snapshot produced by `gateway/internal/openapi.Build()`. This avoids the need for a running gateway in CI and keeps generation deterministic and offline. When the gateway's OpenAPI document changes, update `clients/openapi.json` by running `go test ./gateway/internal/openapi/... -run TestBuildDocument -v` (or equivalent) and committing the output, then re-run `scripts/gen-clients.sh`.

### Generator language

Embedded Python 3 (stdlib only: `json`, `os`, `sys`). Python 3 is present on all common CI images and developer machines. No `oapi-codegen`, `openapi-generator-cli`, or other external tools required.

### Idempotency guarantee

The generator:
- Sorts all map keys (from JSON parsing) consistently using Python 3.7+ insertion-order dicts with explicit `sorted()` calls
- Uses fixed string templates (no timestamps, no randomness)

A second run with the same `clients/openapi.json` produces byte-identical output → `git diff --exit-code clients/` exits 0.

### Go module isolation

`clients/go/go.work` (a local workspace file) overrides the repo-root `go.work` so that `cd clients/go && go build ./...` and `go test -race ./...` work without modifying the shared workspace. The local workspace simply contains `use .`.

### TypeScript: no private-field syntax

Uses TypeScript `private readonly` (compile-time only) instead of `#` native private fields. Both are valid; `private readonly` avoids potential issues with older TypeScript targets or transpilation steps that consumers might apply.

### Error model

Both SDKs model the gateway error envelope `{"error":{"code":"...","message":"...","retryable":true}}` as a concrete typed value:
- **Go**: `*APIError` (implements `error`); callers use `errors.As(err, &apiErr)`.
- **TypeScript**: `ApiError extends Error`; callers use `err instanceof ApiError`.

The `retryable` field is present but optional on the Go side (`bool`, zero-value = false) and `boolean | undefined` on the TypeScript side.

## Running locally

```bash
# Regenerate clients from the committed snapshot (idempotent):
bash scripts/gen-clients.sh

# Check for drift:
git diff --exit-code clients/

# Test Go client:
cd clients/go && go build ./... && go test -race ./...

# Test TypeScript client (first time — installs devDependencies):
cd clients/typescript && npm ci && npm run build && npm test

# Test Python client (first time — installs the package + pytest):
cd clients/python && pip install -e ".[dev]" && pytest -q
```

## Adding a new endpoint

1. Add the route in `gateway/internal/server/routes.go` (per-product edit point).
2. Rebuild the OpenAPI snapshot: run `gateway/internal/openapi.Build()` and capture its JSON output to `clients/openapi.json`.
3. Run `bash scripts/gen-clients.sh` — new typed methods appear in all three SDKs.
4. Commit `clients/openapi.json`, `clients/go/`, `clients/typescript/`, `clients/python/`.

The CI drift job enforces this: if you push a changed spec without regenerating the clients, the `git diff --exit-code clients/` step fails.

## Invariants respected

- `gateway/proto/tool.proto` — untouched.
- `gateway/internal/**` — read-only (only `internal/openapi` is consumed, as input to snapshot creation).
- `workers/sdk-ts/**`, `workers/stubs/ts/**` — untouched.
- `.github/workflows/ci.yml` — untouched; this adds a separate `client-sdk-drift.yml`.
- No new runtime dependency in the gateway Go module or the dashboard.
