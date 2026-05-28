# Directive: Stripe webhook subscription-lifecycle tests

**Date:** 2026-05-28
**Target:** `gateway/internal/billing/webhook.go`
**Motivation:** Regression guard for the "wrong-status downgrade" bug fixed in
`handleSubscriptionDeleted` — non-`canceled` subscription statuses must not
downgrade a customer plan to `free`.

## Required test coverage

| Handler | Scenario | Expected result |
|---------|----------|-----------------|
| `handleSubscriptionDeleted` | `status: "canceled"` | DB updated: `plan_id = 'free'` |
| `handleSubscriptionDeleted` | `status: "past_due"` | Plan **unchanged** (regression guard) |
| `handleSubscriptionUpsert` | `customer.subscription.created` | Plan updated from Stripe price ID |
| Raw webhook endpoint | HMAC mismatch | HTTP 400, zero DB side-effects |

## Patterns to follow

See `gateway/internal/billing/webhook_test.go` for pgxmock + httptest patterns.
Do not add new Go module dependencies.

## Verification

`go test -race ./gateway/internal/billing/...` must exit 0.
