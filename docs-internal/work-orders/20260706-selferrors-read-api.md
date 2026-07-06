# Work order — Customer-facing error-history read API (`selferrors-read-api`)

**Lane:** `10xworker:job` · **Module:** `selferrors-read-api` · **Seeded:** 2026-07-06

## Spec

```json
{
  "module": "selferrors-read-api",
  "scope": [
    "gateway/internal/selferrors/handler.go",
    "gateway/internal/selferrors/store.go",
    "gateway/internal/selferrors/handler_test.go",
    "gateway/internal/selferrors/store_test.go",
    "gateway/internal/server/routes.go",
    "gateway/internal/openapi/openapi.go",
    "gateway/internal/openapi/openapi_test.go"
  ],
  "input": "An authenticated API-key request GET /v1/errors?from=&to=&operation=&code=&page=&limit=, where the write side (errorlog.ErrorRecorder + v1ErrorCapture) has already been populating error_events on every non-2xx /v1 response.",
  "output": "A new gateway-only selferrors package (Store + Handler) exposing the caller's own error_events rows — newest-first, date/operation/code-filtered, paginated — plus a route registration and an additive OpenAPI path item. Customer scope is derived ONLY from auth.FromContext; there is no read path to another customer's rows. Mirrors the shape of the already-merged /v1/keys (#153) and /v1/usage self-service surfaces.",
  "acceptance": [
    "GET /v1/errors is registered in routes.go inside the existing d.DB != nil block, gated by auth.Middleware(d.Auth), using a method-specific registration (r.With(auth.Middleware(d.Auth)).Get, NOT r.Route/Mount) exactly like the /v1/usage note at routes.go:236-257 and webhookDeliveriesHandler at routes.go:203-212, so the static /v1/errors node is matched before the /v1/* invoke wildcard and POST/other methods are not captured.",
    "The handler derives the customer id ONLY from auth.FromContext — never from a path/query/body customer_id — and a test proves customer A cannot read customer B's error_events rows (IDOR guard, mirroring the #150 coverage for /v1/webhooks/deliveries).",
    "The store query mirrors dashboard/app/api/errors/route.ts: WHERE customer_id=$1 AND created_at in [from, toExclusive) with optional operation and code equality/prefix filters, ORDER BY created_at DESC, served via idx_error_events_customer_created; from/to are ISO-date validated, the range is capped (<= 90 days), limit is capped (<= 200), and has_more is computed with a limit+1 probe.",
    "operation and code filters are validated at the boundary (reject malformed with 400) BEFORE the DB query, matching the dashboard OPERATION_FILTER_RE (^/(?:[a-zA-Z0-9_-]+/)*[a-zA-Z0-9_-]+$) and CODE_FILTER_RE (^[A-Z0-9_]{1,128}$).",
    "request_payload (BYTEA, migration 0013) is returned as a bounded UTF-8 string only when present and only for the caller's own rows; the response sets Cache-Control: no-store.",
    "OpenAPI: GET /v1/errors is documented via a NEW errorsPathItems() layered in openapi.Handler (NOT Build), with 200/400/401/500, preserving the TestV1RoutesDriftGuard invoke-route-only invariant that /v1/usage and /v1/keys already respect (openapi.go:596-654). go test -race ./... is green in gateway/ against real Postgres (no mocks)."
  ],
  "forbidden": [
    "No edit to gateway/proto/tool.proto (frozen, invariant #1) — this is a gateway read surface only.",
    "Do not add /v1/errors to openapi Build()'s static path set (that breaks TestV1RoutesDriftGuard); layer it via Handler exactly like usagePathItem/keysPathItems.",
    "Do not accept any customer_id request parameter (path/query/body) — customer scope comes strictly from auth.FromContext, or the endpoint becomes an IDOR.",
    "Do not register the route as r.Route(\"/v1/errors\", ...) — chi Mount claims all methods; use a method-specific r.With(...).Get, per the /v1/usage note at routes.go:236-245.",
    "Do not add a schema/migration — error_events and idx_error_events_customer_created already exist (migrations 0011/0013). Do not mock Postgres or Redis in tests (CLAUDE.md testing rule).",
    "Do not touch internal/auth, internal/billing, internal/usage, or internal/quota — reuse auth.FromContext and the existing pgx pool only."
  ]
}
```

## Rationale

The write side is fully built and already populating `error_events` on every non-2xx `/v1` response
(`errorlog.ErrorRecorder` + `v1ErrorCapture`, wired at routes.go:317; migrations 0011/0013), and the table is
explicitly "for customer-facing error-history inspection" — but headless API-key customers have **no** way to
retrieve it. Only the NextAuth dashboard (`dashboard/app/api/errors/route.ts`) can read it today. This is the
exact "reachable only through the dashboard" gap that motivated the merged `/v1/keys` (#153) and
`/v1/webhooks/endpoints` (#149) self-management jobs, so it is a proven-shaped, high-leverage next phase that
every clone inherits with zero per-product work. It is parallel-safe and byte-disjoint from this cycle's other
work: a brand-new `selferrors` package plus one route line plus an additive OpenAPI layer, touching none of the
respcache, webhook-lifecycle, or channelsig surfaces.
