# 10xworker:job — error-event request-payload capture (opt-in, bounded)

Decomposition spec for a downstream 10X worker. Extends the customer
error-inspection line (`#115` error history + request inspection) so that, when
enabled, a failed `/v1` request's inbound body is captured and surfaced to the
owning customer in the error-history view. **Default-off** so no clone persists
request bodies without an explicit opt-in.

## Spec

```json
{
  "module": "error-event-request-capture",
  "scope": [
    "gateway/migrations/0013_error_events_request_payload.sql",  /* next after 0012_outbound_webhooks.sql; verified free in origin/main */
    "gateway/internal/errorlog/recorder.go",
    "gateway/internal/errorlog/recorder_test.go",
    "gateway/internal/server/routes.go",
    "gateway/internal/config/config.go",
    "dashboard/app/api/errors/route.ts",
    "dashboard/app/dashboard/errors/errors-client.tsx"
  ],
  "input": "the inbound request body plus the existing error context (code, message, request_id, customer_id) on a failed /v1 request",
  "output": "a bounded, opt-in, auth-gated copy of the request payload stored on the error_events row and shown in the customer's own error-history view",
  "acceptance": [
    "New idempotent migration adds a NULLABLE request_payload column to error_events using ADD COLUMN IF NOT EXISTS; re-running on boot is a no-op (migrations run every boot, lexical order, no version table).",
    "Capture is gated by a new config flag that is default-OFF; when off, request_payload is always NULL and no request-body buffering occurs on the hot path (assert in a test).",
    "When on, the request body is buffered before handler dispatch, truncated to a configurable max (default 4 KiB) at a valid UTF-8 code-point boundary with a truncation marker, stored only on 4xx/5xx, and NULL on 2xx.",
    "GET /api/errors returns request_payload ONLY for the authenticated customer's own rows; a test asserts a second customer's rows never expose another customer's payload.",
    "go test -race ./... is green in gateway/ (including internal/errorlog) and pnpm build is green in dashboard/."
  ],
  "forbidden": [
    "No change to error_events PK / UNIQUE / ON DELETE CASCADE semantics, nor to the async non-blocking Record goroutine architecture (Record must stay non-blocking).",
    "request_payload MUST NOT appear in any Prometheus metric label or in access/slog log lines (only in the auth-gated dashboard surface).",
    "The config default MUST be off — no clone captures request bodies without an explicit opt-in.",
    "Do not edit .kimi-review.yml or any reviewer-gate config; do not alter the dashboard<->gateway key-hash parity files."
  ]
}
```

## Rationale

When a customer reports failing calls, the gateway today records operation,
error code, message, and request_id — but not what was sent, so support must ask
for reproduction steps. A bounded, opt-in, auth-gated request-payload copy on the
existing `error_events` row closes that loop and is reusable across every clone
with zero per-product work. Keeping it default-off and strictly bounded preserves
the framework's security posture (no surprise PII persistence, no metric/log
leakage) while making the feature composable with the existing error-inspection
view from `#115`.

## Suggested downstream sequencing

1. Migration `0013_error_events_request_payload.sql` (nullable `BYTEA` column — this project uses PostgreSQL exclusively via pgx/pgxpool; idempotent `ADD COLUMN IF NOT EXISTS`) → verify boot is a no-op on re-run.
2. Config flag (default-off) + max-bytes in `internal/config` → unit test defaults.
3. `errorlog` buffering + truncation + gate on flag/status → `recorder_test.go` covering off/on, 2xx-NULL, 4xx/5xx-stored, truncation marker, UTF-8-safe boundary.
4. `routes.go` wiring to pass the buffered body to `Record` (IO only, stays non-blocking).
5. Dashboard `/api/errors` select + own-rows-only guard test; `errors-client.tsx` opt-in disclosure to view the payload.
