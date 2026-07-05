# Work order — Programmatic customer API-key self-management API (`/v1/keys`)

**Lane:** `10xworker:job` · **Module:** `customer-key-management-api` · **Seeded:** 2026-07-05

## Spec

```json
{
  "module": "customer-key-management-api",
  "scope": [
    "gateway/internal/auth/store.go",
    "gateway/internal/auth/keyshttp.go",
    "gateway/internal/auth/keyshttp_test.go",
    "gateway/internal/server/routes.go",
    "gateway/internal/openapi/openapi.go",
    "clients/openapi.json",
    "clients/**"
  ],
  "input": "An API-key-authenticated (headless, no dashboard login) customer's request to list / rotate / revoke their own API keys, resolved to a customer via auth.FromContext.",
  "output": "An auth-gated, DB-gated /v1/keys subrouter giving API-key customers programmatic parity with the dashboard's NextAuth key management: GET /v1/keys (list active keys, metadata only), POST /v1/keys/{id}/rotate (grace-period rotate, new key shown once), DELETE /v1/keys/{id} (revoke with immediate cache invalidation). Documented in OpenAPI so the client-SDK drift guard stays green.",
  "acceptance": [
    "New SELECT-only Store.List(ctx, customerID) returns only the caller's ACTIVE keys (revoked_at IS NULL AND (expires_at IS NULL OR expires_at > NOW())), fields limited to id/prefix/name/last_used_at/expires_at/created_at; it MUST NOT select or return api_keys.hash.",
    "GET /v1/keys returns only keys owned by auth.FromContext(customer); response body never contains a hash or a full key string.",
    "POST /v1/keys/{id}/rotate calls the existing Store.Rotate (grace window preserved, server-clamped); returns the new full key exactly once with Cache-Control: no-store; the old key still authenticates during the grace window; emits api_key.rotated audit + outbound webhook exactly as Store.Rotate already does.",
    "DELETE /v1/keys/{id} calls the existing Store.Revoke; a cache-warmed key returns 401 on the very next request after revoke (invariant #7 cache DEL), and api_key.revoked audit + webhook fire.",
    "Any {id} not owned by the caller (or nonexistent) returns 404 indistinguishable from not-found (IDOR-safe, mirroring webhookout.DeleteEndpoint).",
    "Routes are registered ONLY inside the d.DB != nil framework block in routes.go (mirroring the /v1/webhooks block at routes.go:198), never inside the per-product V1Routes block.",
    "openapi.go documents GET /v1/keys, POST /v1/keys/{id}/rotate, DELETE /v1/keys/{id} (mirror webhookEndpointsPathItems); clients/openapi.json + generated clients regenerated so the client-sdk-drift CI job is green.",
    "go test -race ./... green against real Postgres + Redis (no mocks); new keyshttp_test.go covers list-scoping/IDOR, rotate grace + secret-once, revoke -> immediate 401."
  ],
  "forbidden": [
    "No bare UPDATE api_keys anywhere; revoke goes through Store.Revoke and rotate through Store.Rotate so the auth:<prefix> Redis cache is invalidated (invariant #7).",
    "No change to auth.Hash, PrefixLen=24, or the base32 alphabet, and no divergence from dashboard/lib/keys.ts (invariants #5, #6).",
    "Never return api_keys.hash or a persisted full key on list/get; the full key is shown exactly once at rotate time only.",
    "No edit to gateway/proto/tool.proto (frozen, invariant #1).",
    "Register in the framework block only; do not touch the per-product V1Routes edit point.",
    "MVP is list + rotate + revoke. POST /v1/keys (minting a NEW persistent key from a possibly-leaked key = self-perpetuation vector) is DEFERRED behind an explicit follow-up decision; do not ship mint in this phase."
  ]
}
```

## Rationale

This is the API-key-path counterpart to the dashboard's magic-link key management — the exact
asymmetry PR #149 closed for webhook **endpoints**, applied to the one remaining account-management
surface still reachable only through the NextAuth dashboard. The dashboard has the full lifecycle
(`dashboard/lib/db.ts` `listKeys`/`revokeApiKey`/`rotateApiKey`), but the gateway `auth.Store`
exposes only `Revoke`/`Rotate`/`Lookup` (no `List`, no HTTP handlers; grep confirms no `/v1/keys`
in `routes.go` or `openapi.go`). A headless customer therefore cannot enumerate, rotate, or revoke
their own keys programmatically. The invariant-respecting primitives already exist and already emit
audit + outbound webhooks, so this phase is almost purely additive HTTP surface over tested
primitives plus one SELECT-only `Store.List`. Framework-level → every clone inherits it. Disjoint
from the concurrent `worker:claim` webhook lane (which the planner deliberately excluded this cycle
to avoid routes.go/openapi.go conflicts with this primary).
