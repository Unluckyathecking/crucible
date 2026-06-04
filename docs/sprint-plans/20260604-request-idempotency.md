# 10X Job Spec — `request-idempotency`

Decomposition seed for the request-idempotency module. The downstream 10X worker
implements against the JSON spec in the PR body; this file is the in-repo record.

## Problem

Crucible is a metered-billing framework whose billable unit is a non-idempotent
`POST /v1/<op>`. On a client retry of a timed-out POST (or after a retryable
`502 WORKER_UNREACHABLE` / `WORKER_BAD_RESPONSE`), the gateway today re-runs the
full pipeline and writes a fresh `usage_events` row — i.e. it **double-bills**.
There is no client-facing idempotency mechanism: `usage_events.request_id` is
gateway-generated and has no unique constraint, and no `Idempotency-Key` handling
exists anywhere in `gateway/`. This is a billing-correctness gap every product
clone inherits.

## Spec

```json
{
  "module": "request-idempotency",
  "scope": [
    "gateway/internal/idempotency/*.go",
    "gateway/migrations/0007_idempotency_keys.sql",
    "gateway/internal/server/routes.go"
  ],
  "input": "Client sends Idempotency-Key header on POST /v1/* ; gateway must dedup retries within a TTL window",
  "output": "First request executes+stores response; identical-key retries replay the stored response without re-invoking the worker or re-billing",
  "acceptance": [
    "New migration 0007 creates idempotency_keys with UNIQUE(customer_id, idempotency_key) and created_at; uses CREATE TABLE IF NOT EXISTS (idempotent, lexical-order safe)",
    "Middleware mounted in the /v1 chain AFTER auth+ratelimit and OUTER relative to quota (registered before quota in the chi Use() chain so replay returns early before quota executes — this satisfies the forbidden constraint 'replay must not reserve or refund quota'); absent Idempotency-Key header is a pass-through (zero behaviour change vs today)",
    "On key hit within TTL the stored status+body is replayed and proxy.Invoke is NOT called and no usage_events row is written (assert worker-invoke count stays 0 on replay)",
    "On first use of a key the response is captured and persisted only for successful (2xx) outcomes; retryable 5xx are NOT cached so a genuine retry can still succeed",
    "Concurrent identical keys never double-invoke (enforced by the UNIQUE constraint + ON CONFLICT); second in-flight returns 409 IDEMPOTENCY_CONFLICT",
    "Same key + different request body returns 422 IDEMPOTENCY_KEY_REUSE (body fingerprint mismatch), never silently replaying a mismatched response",
    "go test -race ./... green in gateway/",
    "Error envelopes use the existing writeJSONError shape (stable code + safe message)"
  ],
  "forbidden": [
    "No change to gateway/proto/tool.proto",
    "No change to the billable_units<1 rejection check in routes.go invoke()",
    "No change to quota reserve/refund signal machinery (quota/middleware.go, quota/context.go) — replay must not reserve or refund",
    "No new server.Deps field that requires a Close()/shutdown hook (reuse existing Redis/PG handles) — keep gateway/cmd/gateway/main.go byte-untouched to stay disjoint from PR #48",
    "No edit to .github/workflows/ci.yml (disjoint from PR #88)",
    "No signature change to NewRedis, proxy.New, or usage.NewRecorder",
    "Do not treat the gateway-generated X-Request-ID as the idempotency key — it is per-request, not client-controlled"
  ]
}
```

## Subunits

1. Migration `0007_idempotency_keys.sql` — idempotent; `UNIQUE(customer_id, idempotency_key)`.
2. Store (`idempotency/store.go`) — claim-or-fetch on `(customer_id, key, body_fingerprint)` via `INSERT ... ON CONFLICT`; persist captured response. Real-PG test.
3. Response-capturing middleware (`idempotency/middleware.go`) — pass-through when header absent; buffer downstream response, persist on 2xx, replay on hit. Race test asserts invoke count == 0 on replay.
4. Wiring line in `routes.go` — one `r.Use(...)` inside `/v1`. Echo route still passes existing tests.
5. TTL/cleanup — bounded retention via `created_at`; stale keys past TTL do not replay.
