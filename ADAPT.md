# Adapting Crucible to a new product

To turn this template into a new API product (e.g. `vat-check`), edit only these places. Everything else is framework infrastructure and stays untouched.

## What you change

1. **`workers/active`** — repoint the symlink to the worker implementing your product. Reuse a language stub (`workers/stubs/<lang>/`) or create a new directory (`workers/<product>/`) implementing the SDK contract.

2. **`gateway/internal/server/routes.go`** — one line per endpoint:
   ```go
   r.Post("/v1/validate-vat", h.Invoke("validate_vat"))
   ```
   The `operation` string is forwarded opaquely to the worker. The framework never needs to know what your product does.

3. **`gateway/migrations/0002_seed_plans.sql`** — define your pricing tiers (rate limit per minute, monthly unit cap, Stripe price id).

4. **`dashboard/app/page.tsx`** — landing copy + pricing display. Replace this page wholesale with your own product's landing/pricing; the shipped page sells Crucible itself and is not meant to survive a clone.

5. **`docs/guides/`** — product-specific MDX docs.

6. **`.env`** — set `API_KEY_PREFIX` (e.g. `vat_`), Stripe keys, worker URL.
   Optionally set `WORKER_SHARED_SECRET` to the same value on **both** the gateway
   and the worker to enable HMAC-SHA256 channel authentication on `/invoke`.
   Leave unset to preserve today's behaviour (no signing). When set, every worker
   that serves this gateway must share the same secret — a mismatch causes all
   calls to be rejected with UNAUTHORIZED.

7. **Stripe dashboard** — create the product + prices matching the migration above.

## What you do NOT touch

- `gateway/proto/tool.proto` — frozen across all clones. The contract is product-agnostic. Wanting to add a field here means you're solving the wrong problem.
- `gateway/internal/{auth,billing,ratelimit,usage,proxy}/` — framework owns these.
- `gateway/internal/audit/` — shared audit emitter; do not fork per product. Every clone writes sensitive actions (key create/revoke, plan changes) through this single `audit.Emit(ctx, db, Event)` call. Adding a product-specific field to `Event` or duplicating the emitter defeats the shared-trail guarantee. If you need extra context, put it in `Event.Details` (JSONB freeform).
- `dashboard/lib/audit.ts` — mirrors the Go emitter field-for-field. Same rule: extend `details`, never fork.
- `workers/sdk-go/` (or other host-lang SDKs) — shared infrastructure.

## The contract

Your worker receives:

```
request_id   — for log correlation
customer_id  — opaque to you
operation    — the routing string from the gateway
payload      — JSON-encoded product input (whatever shape you define)
plan         — "free" | "pro" | "business" | ...
metadata     — key/value flags
```

Your worker returns:

```
payload          — JSON-encoded product output, OR
error            — {code, message, retryable}
billable_units   — ≥ 1. Flat-rate tools return 1. Per-page/per-image/per-token tools return the real unit count.
units_label      — optional, surfaced on invoices ("pages", "images", "tokens", ...).
```

The gateway emits a Stripe `meter_event` with `value=billable_units` for every successful call. Pricing in Stripe is per-unit, not per-call.

Full proto: `gateway/proto/tool.proto`.
