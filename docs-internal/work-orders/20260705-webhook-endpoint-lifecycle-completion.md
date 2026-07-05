# Work order — Webhook endpoint lifecycle completion (PATCH subscription + secret rotation)

**Lane:** `10xworker:job` · **Module:** `webhook-endpoint-lifecycle-completion` · **Seeded:** 2026-07-05

## Spec

```json
{
  "module": "webhook-endpoint-lifecycle-completion",
  "scope": [
    "gateway/internal/webhookout/endpoints.go",
    "gateway/internal/webhookout/endpoints_http.go",
    "gateway/internal/webhookout/endpoints_test.go",
    "gateway/internal/server/routes.go",
    "gateway/internal/openapi/openapi.go",
    "clients/openapi.json",
    "clients/**"
  ],
  "input": "The existing customer-facing webhook-endpoint CRUD on the API-key surface (POST/GET/DELETE /v1/webhooks/endpoints, added by #146/#149), plus the dashboard's updateWebhookEndpointSubscription (dashboard/lib/db.ts:677) as the parity reference.",
  "output": "Two additive endpoints completing the webhook_endpoints resource lifecycle for headless (API-key) customers: PATCH /v1/webhooks/endpoints/{id} to replace an endpoint's subscribed_events set, and POST /v1/webhooks/endpoints/{id}/rotate-secret to issue a fresh signing secret returned exactly once. Reaches dashboard/gateway parity for subscription update and adds a secret-rotation capability neither surface has today. Documented in OpenAPI so the client-SDK drift guard stays green.",
  "acceptance": [
    "New Store fn UpdateEndpointSubscription(ctx, id, events) sets webhook_endpoints.subscribed_events; PATCH /v1/webhooks/endpoints/{id} with {\"subscribed_events\":[...]} returns 204 and persists the new set; an omitted/null subscribed_events resubscribes to ALL events (mirroring dashboard/lib/db.ts:677 semantics, NULL = all).",
    "On subscription narrowing, PATCH deletes now-stale pending/dead_letter webhook_deliveries rows for event types the endpoint is no longer subscribed to (mirror dashboard/lib/db.ts:685-693) — because processDue (emitter.go:217) skips rows whose endpoint no longer matches rather than resolving them, so without cleanup they orphan as perpetual pending.",
    "New Store fn RotateEndpointSecret(ctx, id) generates 32 fresh random bytes via the existing GenerateSecret (emitter.go:145) and UPDATEs the endpoint's secret column; POST /v1/webhooks/endpoints/{id}/rotate-secret returns 200 with the new secret_hex, Cache-Control: no-store; the next delivery signs with the new secret (emitter.go:212) and the old secret no longer verifies.",
    "Both new routes are IDOR-safe: an {id} owned by another customer OR nonexistent returns 404 indistinguishably (match ErrEndpointNotFound convention, endpoints.go:30, per audit #150); no 403 that leaks cross-customer existence.",
    "Both routes require auth context (401 without) and reject a malformed UUID {id} with 400 — matching DeleteEndpointHandler (endpoints_http.go:97-124).",
    "secret_hex appears ONLY in the rotate-secret response; ListEndpoints still never selects or returns the secret column.",
    "openapi.go extends webhookEndpointsPathItems to document PATCH /v1/webhooks/endpoints/{id} and POST /v1/webhooks/endpoints/{id}/rotate-secret; clients/openapi.json + generated clients regenerated so the client-sdk-drift CI job is green.",
    "go test -race ./... green against real Postgres + Redis (no mocks); endpoints_test.go covers PATCH persist + resubscribe-all + stale-row cleanup, rotate secret-once + old-secret-invalidation, and IDOR 404 for both routes."
  ],
  "forbidden": [
    "No change to channelsig Sign/Verify signatures or the outbound header format (X-Crucible-Signature: t=...,v1=...; emitter.go:279-283) — the customer verification contract is frozen.",
    "No edit to gateway/proto/tool.proto (frozen, invariant #1); no billable_units wiring — endpoint management is not a metered/billable operation (matches existing CRUD + /v1/keys).",
    "Do not reverse or touch the Stripe webhook dispatch-first/record-after ordering (invariant #3) or the two-phase flusher (invariant #4) — out of scope.",
    "Rotation is an UPDATE on the existing secret column and subscription update is an UPDATE on subscribed_events — NEITHER needs a schema change. Do not add a speculative migration; if one is unavoidable it MUST be idempotent (invariant #8).",
    "Do not introduce a Redis cache for webhook_endpoints (there is none today; emit/deliver read through Postgres at emitter.go:117-121,212-218). Update/rotate must remain read-through so they take effect on the next delivery with no invalidation dance.",
    "Register both routes ONLY inside the existing r.Route(\"/v1/webhooks\", ...) framework block in routes.go; do not touch the per-product V1Routes edit point."
  ]
}
```

## Rationale

The API-key webhook surface has create/list/delete (#146/#149) but no update and no secret rotation:
a headless customer who wants to change an endpoint's event subscription — or who leaks a signing
secret — can only delete+recreate, minting a new endpoint id and forcing re-registration. The
dashboard has `PATCH /api/webhooks/[id]` (`updateWebhookEndpointSubscription`), so subscription
update is a genuine dashboard/gateway parity gap on the API-key path; secret rotation is missing from
*both* surfaces (`grep -rni "rotat.*secret|RotateEndpoint" gateway/ dashboard/` → 0 hits) and is the
stronger, security-relevant driver. Both edit the same file set, share the IDOR-404 + auth-context
ergonomics of the existing handlers, and complete one resource's lifecycle — so they ship as one
coherent framework-level phase every clone inherits, rather than two separate rounds through
`routes.go`/`openapi.go`. Held last cycle only because #153 was concurrently editing those files;
#153 has merged, so they are free. Disjoint from the concurrent `emit-excludes-inactive-endpoint`
test claim (that lives in `emitter_test.go`).
