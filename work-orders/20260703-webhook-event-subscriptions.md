# 10xworker:job — webhook-event-subscriptions

```json
{
  "module": "webhook-event-subscriptions",
  "scope": [
    "gateway/migrations/0017_webhook_subscriptions.sql",
    "gateway/internal/webhookout/emitter.go",
    "gateway/internal/events/events.go",
    "gateway/internal/openapi/openapi.go",
    "dashboard/app/api/webhooks/route.ts",
    "dashboard/app/api/webhooks/[id]/route.ts",
    "dashboard/lib/db.ts",
    "dashboard/app/dashboard/**/webhooks*",
    "gateway/internal/webhookout/emitter_test.go",
    "dashboard/app/api/webhooks/*.test.ts"
  ],
  "input": "Emitter.Emit currently fans every catalogue event to EVERY active endpoint of a customer via one INSERT…SELECT WHERE customer_id=$ AND active=TRUE (emitter.go:114-117) — there is no per-endpoint event-type filter anywhere in gateway/dashboard/migrations, and webhook_endpoints (0012) has no subscription column.",
  "output": "Per-endpoint event-type subscriptions validated against events.AllEventTypes, so an endpoint only receives the event types it subscribed to, with a backward-compatible 'all events' default for pre-existing rows. An extensible interface that compounds every time the event catalogue (or a clone) adds a new type.",
  "acceptance": [
    "New migration 0017_webhook_subscriptions.sql adds subscription storage (join table or subscribed_events TEXT[] on/alongside webhook_endpoints), idempotent (CREATE TABLE IF NOT EXISTS / ADD COLUMN IF NOT EXISTS / ON CONFLICT DO NOTHING), lexically ordered after 0016.",
    "Emit's INSERT…SELECT gains a subscription predicate: an endpoint NOT subscribed to the emitted event_type produces ZERO webhook_deliveries rows (assert row count in emitter_test.go).",
    "Backward-compat: an endpoint with no explicit subscription still receives all catalogue events — no silent regression for rows created before 0017.",
    "Subscription values are validated against events.AllEventTypes at registration; an unknown event type is rejected with 400 (dashboard API + gateway helper).",
    "Dashboard registration API + UI let the customer choose subscribed types; the secret-shown-once-on-issuance behaviour is unchanged.",
    "openapi.json documents the subscription field; the events↔openapi Build() sync invariant still holds (no panic on boot)."
  ],
  "forbidden": [
    "No per-product fields added to gateway/proto/tool.proto (frozen contract).",
    "Outbound signing scheme unchanged: HMAC-SHA256 over 'ts.body', X-Crucible-Signature, X-Webhook-Event-ID idempotency header.",
    "Do not weaken the egress SSRF guard or the HTTPS/private-host registration validation.",
    "Delivery semantics untouched: at-least-once, backoffSchedule, dead-letter after maxAttempts, stuck-row recovery.",
    "A nil *Emitter must remain a safe no-op on all methods.",
    "Migration stays idempotent, no version-tracking table, no destructive rewrite of existing endpoint rows."
  ]
}
```

The outbound-webhook subsystem is otherwise complete — delivery, retry, dead-letter replay (#142),
SSRF egress guard (#141), outbox wiring (#140) — yet remains all-or-nothing per endpoint. Per-endpoint
event-type subscriptions are the one missing primitive every real webhook consumer needs, and the seam
that downstream phases (subscription-scoped replay, per-type secrets, richer catalogues) all compose on.
Framework-level → every clone inherits it. Verified genuinely unbuilt (no event-type filter in emitter.go's
Emit query; no subscription column in the 0012 schema) and file-disjoint from the open #143 (main.go DB wiring).
