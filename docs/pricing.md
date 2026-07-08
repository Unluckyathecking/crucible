# Pricing & licensing

Crucible is **open core**: the full framework core is MIT-licensed and free to self-host
forever. Paid tiers unlock source-available **Enterprise Edition (EE)** features via an
offline Ed25519 license key (`CRUCIBLE_LICENSE_KEY`) — no phone-home, no runtime dependency
on us.

## Tiers

| Tier | Price | What you get |
|---|---|---|
| **Community** | Free | Self-host the full MIT core: gateway (auth, rate limiting, quota, Stripe metered billing for your customers), worker SDKs (Go/Rust/TS), dashboard, observability stack, docs. |
| **Pro** | £39/month or £390/year | License key unlocks: operator multi-token access, customer audit log export, priority email support, plus all future Pro features. |
| **Business** | £249/month | Everything in Pro, plus: dashboard SSO (OIDC), deployment support, SLA-backed support. Teams/RBAC and multi-project are coming soon. |
| **Enterprise** | Custom | Everything in Business, plus: white-label/embedding rights, SAML (coming soon), compliance documentation, custom terms. |
| **Crucible Cloud** | Waitlist | Hosted control plane — fully managed gateway, dashboard, and metering. In development; [join the waitlist](mailto:sales@crucible.dev?subject=Crucible%20Cloud%20waitlist). |

Community's CTA is the [GitHub repository](https://github.com/Unluckyathecking/crucible).
Pro, Business, and Enterprise are sold via [sales@crucible.dev](mailto:sales@crucible.dev?subject=Crucible%20licence).

## FAQ

**What happens when a license expires?**
EE features enter a 14-day grace period during which they keep working. After the grace
period they disable themselves. The MIT core — gateway, billing for your own customers,
dashboard, workers — is unaffected and keeps running.

**Is this open source?**
The core is open source under the MIT license: clone it, self-host it, ship your product on
it with no strings. Enterprise Edition features are **source-available** — you can read the
source, but running them in production requires a valid license key.

**Do the prices include VAT?**
No. Prices are exclusive of VAT; any applicable tax is added at checkout.
