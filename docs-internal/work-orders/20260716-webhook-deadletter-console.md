# Work order — operator webhook dead-letter replay console

**Date:** 2026-07-16
**Lane:** `10xworker:job` (primary)
**Module slug:** `operator-webhook-deadletter-console`
**Base:** origin/main @ `7efcdb4`

## Problem

The gateway already ships the complete dead-letter operator **API**, mounted under the
operator-token-gated admin surface (`gateway/internal/server/routes.go:429-431`):

- `GET  /v1/admin/webhooks/deadletters?page&per_page` → `webhookout.ListDeadLettersHandler`
- `POST /v1/admin/webhooks/deadletters/{id}/replay`   → `webhookout.ReplaySingleHandler`
- `POST /v1/admin/webhooks/deadletters/replay?endpoint_id=<uuid>` → `webhookout.ReplayBulkHandler`

But there is **no dashboard UI** for it. `find dashboard -path '*operator*' -iname '*webhook*'`
returns nothing, and the operator console nav
(`dashboard/app/operator/_components/operator-nav.tsx`) links only Customers / Audit log /
Plans / Jobs. Dead-letter visibility and replay are therefore reachable only by hand-crafted
curl against an operator token — the one operator subsystem shipped without a console front.

This is the same shape as the already-shipped **operator/jobs** console (which fronts the
analogous `/v1/admin/jobs` requeue/release API), so a ready template exists. Completing it
gives every Crucible clone an inherited dead-letter replay console — reusable across the whole
clone-and-adapt fleet, not a one-off.

## Contract (already on main — do not change the gateway)

`ListDeadLetters` returns `Page[DeadLetterDelivery]` (`gateway/internal/webhookout/replay.go:41`):

```
DeadLetterDelivery { id(string), event_id, event_type, endpoint_id, endpoint_url,
                     endpoint_active, customer_id, attempts, last_response_code?, created_at }
```

- List: `400 BAD_REQUEST "page too large"` when the offset overflows; otherwise paginated,
  most-recent-first, PerPage defaults 20 / clamps 100.
- Replay single (`{id}` = delivery id): `200 {requeued:1}`, `400` invalid id,
  `404 NOT_FOUND` unknown delivery, `409 ENDPOINT_INACTIVE` when the target endpoint is disabled.
- Replay bulk (`?endpoint_id=<uuid>`): `200 {requeued:N}`, `400` invalid/missing endpoint id.

## Deliverable

A `/operator/webhooks` (dead-letters) console page mirroring the operator/jobs pattern exactly:

1. **`dashboard/lib/operator/client.ts`** — add typed client fns + interfaces:
   `DeadLetterDelivery`, `listDeadLetters({page, perPage})` → `Page<DeadLetterDelivery>`,
   `replayDeadLetter(id)` → `{requeued:number}`, `replayEndpointDeadLetters(endpointId)` →
   `{requeued:number}`. Mirror the existing `listAdminJobs` / `requeueJob` / `releaseJobs`
   wire + `OperatorApiError` handling exactly (same `Page<T>` envelope, same error mapping).
2. **`dashboard/app/operator/webhooks/page.tsx`** — `force-dynamic` server component: paginated
   dead-letter table (event type, endpoint URL, attempts, last response code, age), the shared
   `<Pagination>` component, per-row **Replay** button (single by delivery id) and a **Replay all
   for endpoint** action; disable/annotate replay when `endpoint_active === false` and surface the
   `409 ENDPOINT_INACTIVE` message inline (same inline-error convention the jobs page uses for
   400/404). `400 page-too-large` → inline `filterError`, anything else re-throws to the boundary.
3. **`dashboard/app/operator/webhooks/actions.ts`** — server actions
   (`replayDeadLetterAction`, `replayEndpointAction`) mirroring `requeueJobAction`/`releaseJobsAction`:
   `revalidatePath` + redirect back with `?replayed=N` on success, `?error=` on caller-fixable
   400/404/409, re-throw otherwise.
4. **`dashboard/app/operator/_components/operator-nav.tsx`** — add a `Dead-letters` (or `Webhooks`)
   nav link between Plans and Jobs.
5. **`dashboard/app/operator/webhooks/__tests__/page.test.ts`** — mirror
   `operator/jobs/__tests__/page.test.ts`: render with a mocked client (rows, empty state, pagination,
   inline 409 inactive-endpoint case) and cover both server actions' success + error-redirect paths.

## Acceptance

- `dashboard/app/operator/webhooks/page.tsx` renders a paginated dead-letter table sourced from
  `listDeadLetters`; nav link present in `operator-nav.tsx`.
- Single replay posts to `replayDeadLetter(id)` and bulk replay to
  `replayEndpointDeadLetters(endpointId)`; both revalidate + redirect (no bare POST body left in the
  browser), mirroring the jobs actions.
- Inactive-endpoint rows cannot silently 500 the console: the `409 ENDPOINT_INACTIVE` path is
  surfaced inline, proven by a test.
- `client.ts` adds only the three fns + interfaces above; no change to existing exported signatures.
- `pnpm build` green in `dashboard/` (type-check + lint); the new page test suite green.
- **No gateway change** — this fronts the existing admin API only.

## Forbidden

- Do NOT modify any gateway Go code, routes, or the `/v1/admin/webhooks/deadletters*` handlers —
  the API is frozen and correct; this is UI only.
- Do NOT touch `ee/`, `.github/`, or OSS-policy docs (owner drafts #167/#168/#188).
- Do NOT touch `gateway/internal/openapi/openapi.go` or `clients/**` (that is the disjoint
  `worker:claim openapi: document GET /v1/webhooks/deliveries` PR — no overlap).
- Do NOT add a new admin route or widen the operator token scope.
- Do NOT change the shared `Page<T>` / `OperatorApiError` types or the `<Pagination>` component.
