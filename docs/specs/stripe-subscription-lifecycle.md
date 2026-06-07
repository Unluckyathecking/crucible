# stripe-subscription-lifecycle

Self-serve Stripe subscription lifecycle: checkout, customer linkage via webhook, and billing portal access. Closes the monetization gap where the usage flusher filtered `stripe_customer_id IS NOT NULL` but no mechanism existed to populate that column.

## Scope

| File | Change |
|------|--------|
| `gateway/internal/billing/checkout.go` | New: `CheckoutClient` with `CreateCheckoutSession`, `CreatePortalSession`, `LookupStripeCustomerID` |
| `gateway/internal/billing/checkout_test.go` | New: unit tests with httptest Stripe stubs and pgxmock |
| `gateway/internal/billing/webhook.go` | Modified: `checkout.session.completed` + `customer.created` handlers; `CacheDeleter` interface; `WithCacheDeleter` functional option |
| `gateway/internal/billing/webhook_handler_test.go` | Modified: new handler tests + cache invalidation assertions |
| `gateway/internal/auth/inject.go` | New: `auth.WithKey` helper for test context injection |
| `gateway/internal/server/routes.go` | Modified: `BillingService` interface; `/v1/billing/checkout` and `/v1/billing/portal` routes |
| `gateway/migrations/0008_checkout_index.sql` | New: partial index on `customers(email) WHERE stripe_customer_id IS NULL` |
| `dashboard/app/api/billing/checkout/route.ts` | New: POST `/api/billing/checkout` — creates Stripe Checkout Session |
| `dashboard/app/api/billing/portal/route.ts` | New: POST `/api/billing/portal` — creates Stripe Billing Portal Session |
| `dashboard/app/dashboard/billing/page.tsx` | New: Server Component — fetches plan + stripe_customer_id, renders client content |
| `dashboard/app/dashboard/billing/billing-content.tsx` | New: Client Component — `UpgradeButton` and `ManageBillingButton` with loading/error state |
| `dashboard/app/dashboard/page.tsx` | Modified: billing link in the plan section |
| `dashboard/lib/db.ts` | Modified: `getStripeCustomerId` export |

## Design decisions

### No stripe-go SDK

All Stripe API calls use plain `net/http` with `application/x-www-form-urlencoded` bodies. The surface is small (two endpoints) and introducing the SDK would add a dependency the framework explicitly avoids per CLAUDE.md.

### Checkout → webhook → customer linkage

The checkout session carries `client_reference_id = customer.id` (the internal UUID). When Stripe fires `checkout.session.completed`, the webhook handler extracts `client_reference_id` and `customer` (Stripe customer ID) and writes `stripe_customer_id` with an idempotent guard:

```sql
UPDATE customers SET stripe_customer_id = $1
WHERE id = $2 AND (stripe_customer_id IS NULL OR stripe_customer_id = $1)
```

This is safe to replay: if the column is already set to the same value, the UPDATE is a no-op.

### customer.created handler

Handles the case where Stripe fires `customer.created` before `checkout.session.completed`. Looks up the internal customer by email and writes `stripe_customer_id` with the same idempotent guard. Both events are handled so whichever arrives first wins; the second is a no-op.

### Dispatch-before-record (CLAUDE.md invariant #3)

The existing `webhook.go` dispatch-before-dedup-record ordering is preserved unchanged. New handlers slot into the existing switch.

### Cache invalidation (CLAUDE.md invariant #7)

After any `stripe_customer_id` write, the handler queries `api_keys` for all active prefixes belonging to that customer and calls `CacheDeleter.Del("auth:<prefix>", ...)`. This flushes the gateway's Redis hot-cache immediately so the updated plan/customer state is visible on the next request.

`CacheDeleter` is injected via the `WithCacheDeleter` functional option on `NewWebhook`. Main.go's existing 2-arg call is backward-compatible. Tests that pre-date this feature leave `cache = nil`; new tests set `h.cache = spy` explicitly.

### Billing portal 402 guard

`/v1/billing/portal` and `/api/billing/portal` both return `402 NO_STRIPE_CUSTOMER` if `stripe_customer_id` is NULL. The dashboard renders an informational message in that case rather than a broken button.

### Dashboard Server/Client split

`billing/page.tsx` is a Next.js App Router Server Component: it calls `auth()` and queries the DB, then passes `planId` and `hasStripeCustomer` as props to `billing-content.tsx` (the `"use client"` file that holds the interactive buttons). This avoids the incompatible combination of `"use client"` + `export const dynamic` that the initial draft had.

### CSRF defense

Both dashboard API routes use the **double-submit cookie** pattern. The middleware sets a `__csrf` cookie (`SameSite=Strict`, not `HttpOnly`) on every dashboard page load. Client components read this cookie via `document.cookie` and echo it as the `X-CSRF-Token` request header. The route handlers compare the header against the cookie value using a constant-time function (`verifyCsrfToken` in `lib/csrf.ts`) to prevent timing side-channels. An attacker on a different origin cannot read the `SameSite=Strict` cookie, so they cannot forge the matching header. An explicit `Origin` check is retained as defense-in-depth: a present-but-wrong `Origin` is rejected (Safari omits `Origin` on same-origin POSTs, so a null/absent `Origin` is allowed through to the CSRF cookie check).

### Migration 0008

Adds a partial index on `customers(email) WHERE stripe_customer_id IS NULL` to speed up the `customer.created` webhook handler's `WHERE email = $2 AND stripe_customer_id IS NULL` query. Migrations are idempotent (`IF NOT EXISTS`) per CLAUDE.md invariant #8.

## Environment variables

| Variable | Consumer | Purpose |
|----------|----------|---------|
| `STRIPE_SECRET_KEY` | gateway, dashboard | Authenticates Stripe API calls |
| `STRIPE_WEBHOOK_SECRET` | gateway | Verifies webhook HMAC signatures |
| `STRIPE_PRICE_<PLAN_ID_UPPER>` | dashboard checkout route | Maps plan ID to Stripe price ID. Plan ID is uppercased and hyphens replaced with underscores: `pro` → `STRIPE_PRICE_PRO`, `basic-annual` → `STRIPE_PRICE_BASIC_ANNUAL`. Value must match `price_[a-zA-Z0-9_]+`. |
| `NEXTAUTH_URL` / `DASHBOARD_ORIGIN` | dashboard | Constructs success/cancel/return URLs |

## Gateway billing routes

```
POST /v1/billing/checkout   requires Bearer auth; body: {"plan_id": "pro"}
POST /v1/billing/portal     requires Bearer auth; no body
```

Both routes are gated behind `auth.Middleware` and return `{"url": "..."}` on success. If `Deps.Checkout` is nil, both return `503 Service Unavailable`.
