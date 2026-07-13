# 10xworker:job — selfusage-detail-export

Per-event metered usage export for headless (API-key) customers: `GET /v1/usage/events`.
The self-service *read* surface exists for the API-key customer everywhere (`/v1/usage`
summary, `/v1/errors`, `/v1/jobs`, `/v1/keys`, `/v1/webhooks`) **except raw metered
usage line-items**. The only line-item export today is `dashboard/app/api/usage/route.ts`
(session-auth, direct-Postgres) — unreachable by programmatic customers reconciling
against Stripe invoices. This ships the API-key counterpart, structurally a clone of the
merged `selferrors` read-API phase.

```json
{
  "module": "selfusage-detail-export",
  "scope": [
    "gateway/internal/selfusagedetail/**",
    "gateway/internal/server/routes.go",
    "gateway/internal/openapi/openapi.go",
    "gateway/internal/openapi/openapi_test.go",
    "clients/openapi.json",
    "clients/go/**",
    "clients/typescript/**"
  ],
  "input": "API-key-authenticated GET /v1/usage/events?from=&to=&operation=&page=&limit= (and format=csv / Accept: text/csv)",
  "output": "The caller's own usage_events rows (id, operation, billable_units, created_at), newest-first, paginated JSON envelope or RFC-4180 CSV, scoped strictly by auth.FromContext",
  "acceptance": [
    "New gateway/internal/selfusagedetail package (handler.go + store.go + _test.go); go test -race ./... green",
    "Handler derives the customer ONLY from auth.FromContext — no customer_id read from path/query/body",
    "Route registered as r.With(auth.Middleware(d.Auth)).Get(\"/v1/usage/events\", ...) INSIDE the if d.DB != nil framework block, not the per-product r.Route(\"/v1\") POST loop",
    "TestV1RoutesDriftGuard (routes_test.go) still passes (the '/v1 is POST-only' nil-DB router invariant holds)",
    "Reuses paging.ParseQuery/Clamp/Offset and the selferrors date-validation contract (ISO-8601, <=90-day range, no future dates); JSON responses set Cache-Control: no-store",
    "CSV path emits Content-Type: text/csv, RFC-4180-escaped fields, header id,operation,billable_units,created_at",
    "openapi.Build documents GET /v1/usage/events; a second run of bash scripts/gen-clients.sh yields zero git diff (drift-clean); client-sdk-drift.yml stays green"
  ],
  "forbidden": [
    "Do NOT touch gateway/proto/tool.proto (frozen contract)",
    "Do NOT add the route to the per-product r.Route(\"/v1\") POST loop (breaks TestV1RoutesDriftGuard)",
    "Do NOT accept a customer_id from the request — scope solely via auth.FromContext (preserve the IDOR guarantee)",
    "Do NOT meter/bill/quota/rate-limit this endpoint or route it through invoke middleware — it is read-only framework infra",
    "Do NOT add a migration altering usage_events; the covering index idx_usage_detail (migration 0006) already serves these queries",
    "No signature change to cache.NewRedis, auth.Hash/PrefixLen, Store.Revoke, the Stripe webhook dispatch-first ordering, or the flusher batch_id two-phase logic"
  ]
}
```

Highest-leverage next framework increment: it compounds across every clone (zero per-product
edit), composes existing seams (`paging` + `auth` scoping + the `selferrors` template +
the `openapi`/SDK-gen pipeline), and carries zero load-bearing-invariant risk. Does not
overlap the just-shipped async phase (that is `internal/jobs` + async routes; this is a
synchronous read of `usage_events`).
