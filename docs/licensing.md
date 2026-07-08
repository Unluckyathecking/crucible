# Licensing FAQ

Crucible is **open-core**. Most of it is free and open source under the MIT
license. A small set of Enterprise features is **source-available** under a
commercial license and unlocked with a license key. This page explains what that
means for you.

## What's MIT and what's Enterprise?

- **MIT (Community):** the entire framework core — the API gateway, auth, rate
  limiting, Stripe metered billing, quota enforcement, metering, observability,
  OpenAPI/SDK generation, the worker SDKs, and the customer dashboard. This is
  free and open source. You can self-host all of it, forever, with no key.
- **Enterprise Edition (EE):** a small set of add-on features, currently SSO,
  operator multi-token, and customer audit export. These files are
  source-available under the [Crucible Enterprise License](../ee/LICENSE.md) and
  require a license key to run in production.

The boundary is **per file, not per directory**. Every EE file carries a header
naming the Enterprise License. If a file has no such header, it's MIT.

## What happens without a license key?

Nothing breaks. When `CRUCIBLE_LICENSE_KEY` is unset or invalid, the EE features
cleanly disable themselves and Crucible runs as the free Community edition. You
get the full MIT core with no degradation. A key only unlocks the EE add-ons.

## Can I fork it?

**Yes — the core.** The MIT-licensed core is yours to fork, modify, redistribute,
and build commercial products on. That's the whole point of the clone-and-adapt
model.

For the EE files you may view the source, modify them for your own use, and
contribute changes back. You may not redistribute or resell them (see below).

## Can I resell the Enterprise features?

**No.** You may not resell, sublicense, or redistribute the EE files, and you may
not offer the EE features to third parties as a hosted or managed service. You
also may not remove or circumvent the license-key check. These are the core
restrictions of the [Crucible Enterprise License](../ee/LICENSE.md). The MIT core
carries none of these restrictions.

## Is this open source?

The **core is** open source (MIT). The **Enterprise features are not** — they are
**source-available**. You can read and modify the source, but the license retains
commercial restrictions, so it does not meet the Open Source Initiative (OSI)
definition. We follow the same model and terminology as projects like n8n and
Elastic: "source-available", not "open source". Calling it open source would be
inaccurate.

## How does the license key work?

- You set `CRUCIBLE_LICENSE_KEY` in the gateway's environment.
- The key is verified **offline** using an Ed25519 signature. There is **no
  phone-home, no network call, and no telemetry** — your keys and usage never
  leave your infrastructure.
- Each EE feature is gated in code and only activates when the key grants that
  feature.

## What about expiry and grace periods?

When a key expires, there is a **14-day grace period** during which the EE
features keep working. This gives you time to renew without an abrupt cutoff.
After the grace period ends, the EE features disable themselves and Crucible
falls back to the Community edition (the core keeps running normally).

## How do I buy a key?

Editions are **Community** (free) / **Pro** / **Business** / **Enterprise**.

To purchase or renew, contact `licensing@crucible.example` _(placeholder —
replace with the real contact and purchase link before release)_.

## Where are the actual license texts?

- Core (MIT): [`/LICENSE`](../LICENSE)
- Enterprise: [`/ee/LICENSE.md`](../ee/LICENSE.md)
- Enterprise overview for developers: [`/ee/README.md`](../ee/README.md)
