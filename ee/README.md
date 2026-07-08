# Crucible Enterprise Edition (EE)

Crucible is **open-core**. The framework core is MIT-licensed and free to
self-host. A small set of Enterprise features is **source-available** (not open
source) under the [Crucible Enterprise License](LICENSE.md) and is unlocked by a
license key.

Current EE features:

- **SSO** — single sign-on for the dashboard (`sso`).
- **Operator multi-token** — multiple operator tokens with independent scopes
  (`operator_tokens`).
- **Customer audit export** — export the customer-facing audit trail
  (`audit_export`).

Absence of a key is not an error. When `CRUCIBLE_LICENSE_KEY` is unset or
invalid, EE features cleanly disable themselves and Crucible runs as the free
Community edition. The MIT core is unaffected.

## How EE files are marked

The core/EE boundary is **per file**, not per directory. Every source file that
is governed by the Enterprise License carries this header at the top, verbatim:

```
// Crucible Enterprise Edition (EE) file.
// Licensed under the Crucible Enterprise License — see ee/LICENSE.md.
// Not covered by the repository's MIT license.
```

If a file does not carry that header, it is MIT. Do not add or remove the header
casually — it is the legal marker of which license applies.

## How the license key works

- Set `CRUCIBLE_LICENSE_KEY` in the gateway's environment.
- Verification is **offline**: the key is an Ed25519-signed token checked by
  `gateway/internal/license/`. No phone-home, no network call, no telemetry.
- Features are gated in code via `Deps.License.Has("<feature>")` where the
  feature is one of `sso`, `operator_tokens`, `audit_export`.
- After a key expires there is a **14-day grace period** during which EE
  features keep working, giving you time to renew.

## Getting a key

License keys are issued by the copyright holder, Mohammed Ali Bhai.
Editions: **Community** (free, MIT core) / **Pro** / **Business** /
**Enterprise**.

To request or renew a key, contact: `licensing@crucible.example` _(placeholder —
replace with the real contact before release)_.

## Editions at a glance

| Edition | License | Key required | What you get |
|---|---|---|---|
| Community | MIT | No | Full framework core, self-hosted |
| Pro / Business / Enterprise | Enterprise License | Yes | Core + EE features (SSO, operator multi-token, audit export) |

See [`docs/licensing.md`](../docs/licensing.md) for the full customer FAQ.
