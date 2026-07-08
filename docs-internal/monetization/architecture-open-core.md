# Open-core architecture & module decomposition (PRIVATE)

Status: adopted 2026-07-08. Companion strategy doc: `strategy-gtm.md`.
This is the implementation record for the open-core transition; it is the spec
the M1–M5 modules were built against.

## 1. Design constraints (from the existing codebase)

- The `plans` table + `billing.PlanCache` tier END-customers of a cloned product
  (quantitative: rate/min, monthly cap). Crucible's own monetization is a second,
  independent axis: **deployment-level entitlements**. The two never mix; no
  entitlement data goes into `plans`, no plan data into the license.
- All CLAUDE.md invariants hold: frozen proto, billable_units floor, webhook
  ordering, flusher batch_id, Go/TS hash mirror, PrefixLen 24, Store.Revoke,
  idempotent migrations. The license layer is purely additive.
- EE boundary is **per-file headers** (GitLab/Sourcegraph style), not an `ee/`
  package tree — Go package layout stays idiomatic. `ee/` holds only LICENSE.md
  and README.md.
- Community edition must work with zero new configuration. No key → community →
  EE endpoints return 403 `FEATURE_NOT_LICENSED` (apierror envelope) or are
  simply not offered in the dashboard UI.

## 2. License key mechanism (M1 — `gateway/internal/license/`)

- Format: `cru1.<base64url-nopad(payload JSON)>.<base64url-nopad(ed25519 sig over raw payload bytes)>`
- Payload: `id, licensee, email, edition (pro|business|enterprise), features [],
  seats, issued_at, expires_at (RFC3339)`.
- Empty `features` derives edition defaults: pro → [operator_tokens, audit_export];
  business/enterprise → [sso, operator_tokens, audit_export].
- Expiry: 14-day grace (`InGrace()` true, warning logged), past grace → invalid.
- Verification is offline: public key from `CRUCIBLE_LICENSE_PUBKEY` (hex) or the
  embedded `DefaultPublicKeyHex`. **The embedded dev key must be replaced (or
  env-overridden) with a production key generated offline before selling.**
- Invalid/absent key never crashes the gateway: log + fall back to community.
- API: `license.Parse(raw, pub) (*License, error)`; `(*License).Has(feature)` and
  `InGrace()` are nil-safe (nil == community). Feature constants: `FeatureSSO`
  ("sso"), `FeatureOperatorTokens` ("operator_tokens"), `FeatureAuditExport`
  ("audit_export").
- Wiring: parsed once in `cmd/gateway/main.go`, logged at startup, injected as
  `server.Deps.License`.
- Issuance: `gateway/cmd/licensegen` — `keygen` / `sign` / `verify`.
- TS mirror: `dashboard/lib/license.ts` mirrors parse+verify semantics (same
  precedent as the keys.go/keys.ts hash mirror). Byte-identical format rules.

## 3. First EE features

| Feature | Gate | Where |
|---|---|---|
| Operator multi-token | `operator_tokens` | Builds out the pre-scaffolded `operator_tokens` table (migration 0015): DB-backed named/revocable tokens accepted by `operator/middleware.go` alongside the static `OPERATOR_TOKEN`; admin CRUD under `/v1/admin/tokens`. |
| Customer audit export | `audit_export` | `GET /v1/audit` self-service endpoint mirroring `selferrors/handler.go`: customer-scoped rows from `audit_log`, `paging` envelope. |
| Dashboard SSO (OIDC) | `sso` | OIDC provider in `dashboard/auth.config.ts`, enabled only when license valid + `SSO_OIDC_ISSUER/CLIENT_ID/CLIENT_SECRET` set; conditional SSO button on login page. |

EE file header (exact text, stamped on every EE implementation file):

```
// Crucible Enterprise Edition (EE) file.
// Licensed under the Crucible Enterprise License — see ee/LICENSE.md.
// Not covered by the repository's MIT license.
```

## 4. Module decomposition & execution

Built by parallel agents in isolated worktrees, merged in dependency order:

| Module | Scope | Depends on |
|---|---|---|
| M1 | license package + licensegen CLI + config/main/Deps wiring | — |
| M2 | ee/LICENSE.md, ee/README.md, root LICENSE preamble, README editions section, docs/licensing.md, CLAUDE.md + ADAPT.md licensing invariants | — |
| M5 | dashboard landing page (sells Crucible), docs/pricing.md | — |
| M3 | operator multi-token + `GET /v1/audit` (Go, EE-gated) | M1 |
| M4 | dashboard SSO + lib/license.ts mirror (TS, EE-gated) | M1 |

Merge order: M1 → M2 → M5 → M3 → M4, integration test after each Go-touching
merge (`go test -race ./...`), `pnpm build` after each dashboard-touching merge.

## 5. New invariants (added to CLAUDE.md by M2)

1. EE files carry the header; EE behaviour is gated via `Deps.License.Has(...)`.
2. Never remove or bypass license checks; never gate quantitative limits.
3. The license axis and the plans axis stay separate.
4. `dashboard/lib/license.ts` mirrors `gateway/internal/license` semantics the
   same way keys.ts mirrors keys.go.

## 6. Explicitly deferred (roadmap, do not build yet)

- **Teams/RBAC, multi-project/org model**: requires new `organizations`/`projects`
  tables and re-parenting `api_keys`/`usage_events` — substantial schema change;
  gate on paying demand.
- **Crucible Cloud control plane**: multi-tenant hosting; schema is single-tenant
  today. Gate on waitlist evidence (strategy doc Phase 3).
- **Automated license issuance portal**: manual issuance until ~20 customers;
  when built, build it as a Crucible product (license issuance as a metered API).
- **SAML**: after OIDC proves out; enterprise-only.
