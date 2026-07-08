# Crucible monetization strategy & GTM roadmap (PRIVATE)

Status: adopted 2026-07-08. This doc is internal — do not publish, do not quote in public docs.
Companion technical doc: `architecture-open-core.md` (same directory).

## 1. The decision

Crucible moves from "MIT template you clone" to **open-core**:

- **Crucible Core stays MIT.** Everything shipped through v1 (gateway, worker SDKs,
  dashboard, observability, docs, deploy scripts) remains free and self-hostable.
  Nothing already released is clawed back — old MIT copies exist regardless, and
  clawbacks poison trust before the product has any.
- **New Enterprise Edition (EE) features are source-available** under the Crucible
  Enterprise License (`ee/LICENSE.md`), unlocked by an offline Ed25519 license key
  (`CRUCIBLE_LICENSE_KEY`). No phone-home, no telemetry — verification is entirely
  local. EE files are marked with a per-file license header, not a separate repo.
- **Crucible Cloud (hosted control plane) is roadmap, not build.** The schema is
  single-tenant (one deployment = one product = one customer base); multi-tenant
  hosting is a Phase-3 bet gated on demand evidence (waitlist signups), not
  something to speculatively build.

Why this model and not alternatives considered:

- *Paid repo access*: nobody pays for permission to read a repo; MIT copies of v1
  are already permissive forever. Dead end.
- *Full relicense to fair-code*: legally clean (single author, 50/50 commits) but
  hostile signal at zero-traction stage; restrictive licensing before anyone cares
  creates suspicion, not revenue (n8n's own framing).
- *Hosted-cloud-first*: highest willingness-to-pay but the codebase has no tenant
  isolation; building it before demand evidence is the classic premature platform
  mistake.

The line we sell: **"You can build with Crucible for free. You pay when Crucible
saves you production, devops, or team time."**

## 2. Editions & pricing

| Edition | Price | What it includes |
|---|---|---|
| **Community** | Free (MIT) | Full core: gateway (auth, rate limit, quota, Stripe metered billing for *your* customers), worker SDKs (Go/Rust/TS), dashboard, observability stack, deploy scripts, docs. |
| **Pro** | £39/mo or £390/yr | License key. Operator multi-token access, customer audit-log export, priority email support. All future Pro features. |
| **Business** | £249/mo | Pro + dashboard SSO (OIDC), deployment support, SLA-backed support. Teams/RBAC and multi-project when built. |
| **Enterprise** | Custom | Business + white-label/embedding rights, SAML (later), compliance docs, custom terms. |
| **Cloud** | Waitlist | Hosted control plane. Existence of the waitlist IS the demand experiment. |

Pricing posture: deliberately simple, deliberately cheap at the bottom. The goal
for the first 6 months is discovering what people pay for, not maximizing ARPU.
Annual = 2 months free. Prices exclude VAT. Repricing is expected and fine before
~20 paying customers; grandfather early buyers for 12 months when it happens.

Feature → entitlement mapping (enforced by `license.Has`):

| Feature key | Pro | Business | Enterprise |
|---|---|---|---|
| `operator_tokens` | ✅ | ✅ | ✅ |
| `audit_export` | ✅ | ✅ | ✅ |
| `sso` | — | ✅ | ✅ |

Rule for future features: **quantitative limits are never license-gated** (rate
limits/quotas belong to the product's own plan tiers); **capabilities that save
team/ops/compliance time are EE**. Candidates in order of likely willingness-to-pay:
teams/RBAC on the dashboard, multi-project/org model, SAML, log streaming, private
template library, longer usage retention, AI-assisted worker scaffolding.

## 3. GTM roadmap

### Phase 0 — Foundation (now; this branch)
- License mechanism, licensegen CLI, EE license text, per-file headers.
- First three EE features (operator tokens, audit export, SSO).
- Public pricing page + docs/licensing.md + README repositioning.
- Exit criteria: `go test -race` green, `pnpm build` green, a license can be
  issued with `licensegen sign` and flips features on a running gateway.

### Phase 1 — Sellable (weeks 1–4)
- Generate the production keypair OFFLINE (`licensegen keygen`); store the private
  key in a password manager, never on a dev machine repo. Replace the embedded dev
  public key.
- Payment rail: Stripe Payment Links (one per Pro monthly/annual, Business monthly)
  → manual license issuance by email within 24h. Do NOT build automated issuance
  yet; at <20 customers manual is faster than software.
- Landing: point crucible domain (buy one) at the dashboard landing page or a
  static export of it. Set up sales@ and a waitlist@ (or form) for Cloud.
- Docs pass: quickstart must take a stranger <30 min from clone to metered API.
- Ship one real public product built on Crucible (the VIES VAT worker is ready) —
  it is the living demo and the credibility anchor.

### Phase 2 — Distribution (months 2–4)
- Positioning to lead with: "idea → secure, metered, billed API in a day". The
  buyer is a solo builder / small team shipping paid APIs, allergic to writing
  billing/auth/quota plumbing again.
- Channels, in order of expected ROI:
  1. Show HN / lobste.rs launch of the open-source core (not the paid tiers).
  2. Build-in-public thread series: the VIES product's real Stripe revenue.
  3. Comparison/SEO pages: vs rolling your own, vs Kong+Stripe glue, vs Zuplo/
     Speakeasy-style hosted gateways.
  4. Worker SDK templates for popular niches (LLM proxy, PDF ops, data validation)
     — each template is a top-of-funnel artifact.
- Success metric to watch: clones→deploys→license inquiries funnel. Instrument
  nothing invasive; count GitHub stars/clones, waitlist signups, sales@ volume.

### Phase 3 — Leverage (months 4+, gated on evidence)
- If Cloud waitlist > ~50 credible signups → spec the multi-tenant control plane
  (new org/project schema, per-tenant isolation — see architecture doc §6).
- If self-host Pro sells but Cloud doesn't → double down on EE: teams/RBAC, SAML,
  automated license issuance portal (a Crucible-on-Crucible product: license
  issuance IS a metered API).
- If neither sells by month 6 → the framework remains the engine for first-party
  API products (original thesis); monetization reverts to shipping products on it.

## 4. Sales/ops mechanics

- License issuance: `licensegen sign --licensee "Acme Ltd" --email ops@acme.com
  --edition pro --expires <+1y>`. Record every issued license (id, licensee,
  edition, expiry, price paid) in a private ledger (spreadsheet is fine at this
  stage; NOT in this repo).
- Renewals: expiry + 14-day grace is enforced in code; calendar-reminder renewals
  manually until volume justifies automation.
- Refund policy: 14 days no-questions. Keep it on the pricing page.
- Support: priority email = 1 business day (Pro), SLA (Business) = defined in
  order form, not marketing page.

## 5. Risks & honest caveats

- **MIT core can be forked and EE features reimplemented.** True of n8n/GitLab
  too. The moat is velocity + trust + the boring correctness work (see
  docs-internal/REVIEW.md), not the license.
- **License checks in MIT core can be legally stripped** — but the EE feature
  *implementations* carry the EE header, so using them without a key breaches
  ee/LICENSE.md regardless. Enforcement is reputational at this scale; that's fine.
- **Two-axis confusion risk**: Crucible's license (operator-level) vs the plans
  table (end-customer tiers of a cloned product). Docs must keep these visually
  separate everywhere; conflating them is the #1 support-confusion risk.
- **Solo-maintainer bus factor** is the real enterprise objection; don't sell
  Enterprise hard until there's a support story.
