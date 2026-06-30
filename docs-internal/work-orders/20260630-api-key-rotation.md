# Work order — API key rotation with grace-period expiry

**Lane:** `10xworker:job` · **Module:** `api-key-rotation` · **Seeded:** 2026-06-30

## Spec

```json
{
  "module": "api-key-rotation",
  "scope": [
    "gateway/migrations/0014_api_key_expiry.sql",
    "gateway/internal/auth/store.go",
    "gateway/internal/auth/store_test.go",
    "dashboard/lib/db.ts",
    "dashboard/app/api/keys/[id]/rotate/route.ts",
    "dashboard/app/api/keys/[id]/rotate/__tests__/**",
    "dashboard/app/dashboard/**"
  ],
  "input": "An authenticated customer's request to rotate one existing active key (key id + optional grace-window seconds, server-clamped).",
  "output": "A freshly issued full key (shown once) with the old key's expires_at set to now+grace; both keys authenticate during the grace window, after which only the new key works.",
  "acceptance": [
    "Migration 0014 adds nullable expires_at TIMESTAMPTZ to api_keys via ALTER TABLE ... ADD COLUMN IF NOT EXISTS plus a partial index, and is idempotent on every boot (invariant #8).",
    "Store.Lookup cold-path SQL excludes expired keys (AND (expires_at IS NULL OR expires_at > NOW())) AND the Redis cacheEntry carries expires_at so the hot path rejects an expired-but-cached key without waiting for the 60s TTL.",
    "Store.Rotate (and dashboard equivalent) issues a new key, sets the old key's expires_at, and fires the auth:<prefix> cache DEL for the old prefix (same best-effort invalidation as Revoke).",
    "A test proves: old key valid before grace end, both keys valid during grace, old key returns ErrKeyNotFound after expires_at while the new key still authenticates.",
    "Rotation writes an audit.Emit event using Event.Details (not a new Event field), mirroring create/revoke.",
    "Go Hash/PrefixLen/base32 generation are reused unchanged; the new key is issued via the existing Generate/hashKey path."
  ],
  "forbidden": [
    "No expiry/rotation field on gateway/proto/tool.proto (frozen contract, invariant #1).",
    "No bare UPDATE api_keys for expiry; go through a Store.Rotate / revokeApiKey-style helper that fires auth:<prefix> DEL (invariant #7).",
    "No change to PrefixLen, base32 alphabet, or Go/TS hash parity (invariants #5, #6).",
    "No forking the audit emitter or adding a product-specific Event field; use audit.Emit with Details JSONB.",
    "Not a per-product edit point: lands in framework auth/dashboard, never in routes.go's per-product block."
  ]
}
```

## Rationale

Crucible's 10X cadence has saturated the observability / contract / resilience axes (#100–#135), but credential lifecycle stopped at binary revoke. Production customers need to rotate keys without a downtime window, and the framework already has every prerequisite — multiple active keys per customer, parity'd hashing, the `auth:<prefix>` invalidation pattern, and the shared audit emitter. Rotation is a small, self-contained, reusable primitive that compounds across every clone and lives entirely in framework-owned surfaces.

## Greenfield evidence

- No `expires_at` / `rotate` / `RotateKey` concept anywhere in `gateway/` or `dashboard/`; `api_keys` (0001) carries only `revoked_at`.
- Migrations stop at `0013`; `0014` is the next free slot.
- `Store` exposes `Revoke`/`Lookup` only — no `Rotate`; Lookup SQL filters `revoked_at IS NULL` with no time predicate.

## Decomposition (downstream sub-units)

1. **Schema** — `0014_api_key_expiry.sql`: nullable `expires_at` + partial index; idempotent.
2. **Lookup correctness** — extend cold-path SQL + carry `expires_at` in the Redis `cacheEntry` so the hot path enforces expiry immediately (closes the up-to-60s stale-valid window).
3. **Store.Rotate** — issue + set old `expires_at` + best-effort `auth:<prefix>` DEL; audit via `Details`.
4. **Dashboard** — `rotateApiKey` in `lib/db.ts` (mirrors `revokeApiKey` incl. cache DEL), `POST /api/keys/[id]/rotate`, and the keys-view rotate affordance.
5. **Tests** — Go store_test grace-window transition (real PG+Redis); dashboard route test.
