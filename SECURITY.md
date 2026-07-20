# Security policy

## Reporting a vulnerability

Use GitHub's private vulnerability reporting: open the repo's [Security tab](https://github.com/Unluckyathecking/crucible/security) and choose "Report a vulnerability". Please don't open a public issue for anything you believe is exploitable.

You'll get an acknowledgment once the report has been read, and either a fix or a reasoned dismissal after triage. This is a one-maintainer project; there is no promised response time.

## Scope

The attack surface worth probing:

- API key issuance, hashing and verification (`gateway/internal/auth`, `dashboard/lib/keys.ts`)
- Billing: Stripe webhook signature verification, metering, the usage flusher
- Outbound webhook delivery — SSRF through customer-registered endpoint URLs (`gateway/internal/egress` is the guard)
- Rate-limit or quota bypasses that let usage go unmetered
- The operator API (`/v1/admin`) auth boundary

Weak defaults in example configs or the dev seed script are better filed as regular issues.

## Supported versions

The `main` branch of this template. Clones are independent copies: once you've cloned, you pull fixes into your copy yourself — there is no backport channel.
