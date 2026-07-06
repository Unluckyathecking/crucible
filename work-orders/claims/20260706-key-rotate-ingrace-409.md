# worker:claim — Rotating an in-grace API key returns a misleading 404 (should be 409)

**Lane:** `worker:claim` · **Seeded:** 2026-07-06

**Target:** `gateway/internal/auth/store.go`, `gateway/internal/auth/keyshttp.go`,
`gateway/internal/auth/keyshttp_test.go` (+ `store_test.go`). Small, bounded reliability/UX fix.

**Problem (real, deterministic — not a race):**
- `Store.List` returns in-grace keys — its filter is `revoked_at IS NULL AND (expires_at IS NULL OR
  expires_at > NOW())` (`store.go:376-377`) — so a key already rotated once (its `expires_at` set, still
  inside the grace window) **is shown** by `GET /v1/keys`.
- `Store.Owner` checks only `revoked_at IS NULL` (`store.go:406`), so that same in-grace key passes the
  ownership gate and `ownedKeyID` returns `ok=true` (`keyshttp.go:88-113`).
- But `Store.Rotate`'s locking SELECT requires `expires_at IS NULL` (`store.go:206-208`), so it returns
  `ErrKeyNotFound` for an in-grace key, which `RotateKeysHandler` maps to `404 "api key not found"` with a
  comment calling it a "benign race" (`keyshttp.go:155-160`). It is not a race: a **listed, owned, in-grace**
  key deterministically 404s on re-rotate, which reads as "your key vanished."

**Directive:** Distinguish "row exists but already has `expires_at` set (in grace, rotation already
happened)" from a genuine not-found in `Store.Rotate` — return a distinct sentinel (e.g. `ErrKeyRotating`).
Map that sentinel in `RotateKeysHandler` to `409 CONFLICT` with a clear message (e.g. "key already rotated;
in grace period"). A genuinely absent / cross-customer id must still return the IDOR-safe 404 unchanged.

**Acceptance:** a list-then-rotate on an in-grace key returns **409**, not 404, with a stable error code +
safe message; a nonexistent id and a cross-customer id both still return 404 (IDOR-safe); the existing
happy-path rotate is unchanged. `go test -race ./...` green in `gateway/` against real Postgres.

**Constraints:** Bounded to the auth key-lifecycle files above. Do not change `PrefixLen`, the
`Hash(salt||key)` semantics, or `Store.Revoke`'s cache-DEL path (invariants #5/#6/#7). No schema change.
**Parallel-safe** and byte-disjoint from the `response-result-cache` primary (respcache/routes/openapi/config)
this cycle.
