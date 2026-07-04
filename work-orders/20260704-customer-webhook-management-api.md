# 10x work order — customer-webhook-management-api

Programmatic, API-key-authenticated CRUD tier for outbound webhook **endpoints**
(`POST/GET/DELETE /v1/webhooks/endpoints`) — the write-counterpart to the read
surfaces that just shipped (`GET /v1/usage` #144, `GET /v1/webhooks/deliveries`,
per-endpoint subscriptions #146).

## Spec

```json
{
  "module": "customer-webhook-management-api",
  "scope": [
    "gateway/internal/webhookout/endpoints.go",
    "gateway/internal/webhookout/endpoints_http.go",
    "gateway/internal/webhookout/endpoints_test.go",
    "gateway/internal/server/routes.go",
    "gateway/internal/openapi/openapi.go"
  ],
  "input": "API-key-authenticated customer requests to register/list/delete outbound webhook endpoints (url, subscribed_events)",
  "output": "POST/GET/DELETE /v1/webhooks/endpoints in the framework (DB-gated) block; customer-scoped store CRUD reusing egress.Blocked + events.IsValidEventType; signing secret returned exactly once; OpenAPI documents the three paths",
  "acceptance": [
    "POST/GET/DELETE /v1/webhooks/endpoints registered in the framework block of routes.go gated on d.DB != nil (mirrors GET /v1/webhooks/deliveries), never in the per-product V1Routes loop",
    "Create rejects non-https:// and private/loopback/link-local hosts -> 400 (reuse egress.Blocked for IP literals, the Go mirror of the dashboard isPrivateHostname); unknown subscribed_events entry -> 400 via events.IsValidEventType",
    "Create returns the signing secret exactly once in the response body; the list handler row struct has no secret field (test asserts secret bytes are never serialized)",
    "DELETE of an endpoint id owned by another customer returns 404 (IDOR-safe), asserted in a store/handler test; every query scoped by auth.FromContext customer id",
    "openapi.json documents the three paths and the events<->openapi Build() sync invariant still holds (no boot panic); go test -race ./... green in gateway/"
  ],
  "forbidden": [
    "no fields added to gateway/proto/tool.proto (frozen)",
    "do not modify emitter.go delivery semantics (at-least-once, FOR UPDATE SKIP LOCKED claim, backoff, dead-letter) or the outbound HMAC scheme (ts.body, X-Crucible-Signature)",
    "do not weaken the egress SSRF guard or the delivery-time GuardedTransport",
    "do not touch the existing GET /v1/webhooks/deliveries handler",
    "signing secret is BYTEA, shown exactly once, never re-returnable",
    "no new migration — 0012 + 0017 already provide url, secret BYTEA, active, subscribed_events TEXT[]"
  ]
}
```

## Why this is the next phase

The recent trajectory built every webhook primitive **except** programmatic
provisioning: delivery/retry/dead-letter (#116/#140), replay (#142), SSRF egress
guard (#141), per-endpoint subscriptions (#146), and a customer read endpoint
`GET /v1/webhooks/deliveries`. Endpoint *lifecycle* is reachable only through the
NextAuth dashboard (`dashboard/app/api/webhooks/route.ts`); no production gateway Go
code inserts/updates/deletes `webhook_endpoints` (every hit is a SELECT/JOIN or a
`*_test.go` seed). A developer holding only an API key cannot register a webhook.
This is framework-level → every clone inherits programmatic webhook provisioning,
and it composes directly on #146 (`subscribed_events`) and #141 (egress guard).
Disjoint from open #143 (main.go DB wiring): this only touches routes.go/openapi
plus new files.
