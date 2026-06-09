# Spec — `unified-error-response-envelope`

Primary `10xworker:job` decomposition. Consolidate the gateway's six hand-rolled
error-body writers into one reusable `internal/apierror` package so every clone
inherits a single, correlatable error contract.

## Problem (verified against `main`)

The customer-facing error envelope `{"error":{"code","message","retryable"}}` is
hand-rolled in six places with three divergent shapes:

| Site | Shape |
|---|---|
| `internal/server/routes.go:358` `writeJSONError` | has `retryable` |
| `internal/idempotency/middleware.go:253` `writeIDError` | near-duplicate; comment admits it copies "the gateway's stable shape" |
| `internal/auth/middleware.go:40,68` | omits `retryable` |
| `internal/ratelimit/middleware.go:43` | raw `[]byte` literal, no `retryable` |
| `internal/quota/middleware.go:89` | raw `[]byte` literal, no `retryable` |
| `internal/middleware/middleware.go:59` (Recovery) | omits `retryable`; `rid` extracted at `:52` but only logged |

The drift is already documented as a known wart at `internal/openapi/openapi.go:199-201`
("`retryable` ... may be omitted by upstream/auth layers, so it is not required in
the schema"). No error body echoes `request_id`, so support cannot correlate a
customer-reported error code to a gateway log line. This is pure framework
infrastructure (no per-product edit point), so the consolidation compounds across
every clone.

## Module

- **Input:** six divergent `{"error":{...}}` writers across the gateway.
- **Output:** one `internal/apierror` package — a single
  `Write(w, requestID, status, code, message, retryable)` emitter with named
  error-code constants, an always-present `retryable`, and a `request_id` field
  echoed from context; all six call sites and the OpenAPI `Error` schema route
  through it.

## Decomposition (7 subunits)

1. **`internal/apierror` package skeleton** — `Error` struct, `Write`
   func `(w, requestID, status, code, message, retryable)`, and exported code
   constants (`UNAUTHORIZED`, `INTERNAL`, `RATE_LIMITED`, `QUOTA_EXCEEDED`,
   `BAD_REQUEST`, `WORKER_UNREACHABLE`, `WORKER_BAD_RESPONSE`, `STRIPE_ERROR`,
   `NOT_CONFIGURED`, `PLAN_NOT_FOUND`, `NO_STRIPE_CUSTOMER`). The writer takes the
   request id as a **string argument** — it does not import `middleware`/`httputil`
   (no import cycle); callers extract the id from context and pass it.
2. **`routes.go`** — delete `writeJSONError`; route all call sites through
   `apierror.Write`. Codes stay byte-identical.
3. **`auth/middleware.go`** — emit via `apierror`; bodies now carry `retryable`
   and `request_id`.
4. **`ratelimit/middleware.go`** — replace the raw `[]byte` literal with `apierror`.
5. **`quota/middleware.go`** — replace the raw `[]byte` literal with `apierror`.
6. **`idempotency/middleware.go` + `middleware/middleware.go` (Recovery)** — delete
   `writeIDError`; Recovery returns the already-extracted `rid` in the body.
7. **`openapi/openapi.go` + `apierror/*_test.go`** — add `retryable` + `request_id`
   to the `Error` schema (mark `retryable` required); remove the stale
   `:199-201` comment. Tests assert envelope shape, all-fields-present,
   `retryable` true/false, `request_id` passthrough, and headers.

## Acceptance

- New package `internal/apierror/` exists with `Error`, `Write`, and the code
  constants above.
- `routes.go::writeJSONError` and `idempotency::writeIDError` are deleted (grep for
  either returns zero non-import hits).
- No remaining raw `[]byte("{\"error\":...}")` literals or inline
  `map[string]any{"error":...}` envelopes in auth/ratelimit/quota/recovery.
- Every emitted error body contains `code`, `message`, `retryable`, `request_id`
  (non-empty `request_id` on the auth/quota/ratelimit/recovery paths).
- Recovery returns the extracted `rid` (previously logged-only at
  `middleware.go:52`).
- OpenAPI `Error` schema adds `retryable` + `request_id`; `retryable` is required;
  stale `:199-201` comment removed.
- `go test -race ./...` green in `gateway/`; no new import cycle.

## Forbidden

- Editing `cmd/gateway/main.go` or `internal/auth/store.go` (PR #48 parallel-safety).
- Changing any error **code** string a customer currently receives — consolidate the
  writer only; keep `RATE_LIMITED`/`QUOTA_EXCEEDED`/`UNAUTHORIZED`/etc. byte-identical.
- Touching `gateway/proto/tool.proto` or worker contract surfaces.
- Adding `request_id` to **success** bodies (only error bodies; the `X-Request-ID`
  header already covers success).
- Placing the writer in `internal/httputil` (would create an
  `httputil→middleware→httputil` cycle via `RequestIDKey`).
- Widening into a logging/observability refactor — log lines stay as-is; this is
  response-body consolidation only.
