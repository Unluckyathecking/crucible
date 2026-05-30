# Plan: dashboard-magic-link-prod-guard

**Sprint:** 2026-05-26

## Context

PR #8 "Fix magic link exposure in logs" was closed without merging on 2026-05-25. The issue remains live: `dashboard/auth.ts` has a fallback path that calls `console.log(url)` (where `url` is the magic-link authentication URL) when `RESEND_API_KEY` is absent. This path is reachable in production if the env var is accidentally unset, causing authentication tokens to appear in stdout/container logs — a credential-leak risk.

The development fallback (log to console when running locally) is intentional and must be preserved.

## Scope

`dashboard/auth.ts` only.

## What to implement

Wrap the existing fallback block with an environment check:

```typescript
// Before (current code, simplified):
if (!process.env.RESEND_API_KEY) {
  console.log(`Magic link: ${url}`);
  return;
}

// After:
if (!process.env.RESEND_API_KEY) {
  if (process.env.NODE_ENV === 'production') {
    throw new Error('RESEND_API_KEY is required in production — magic link not sent');
  }
  console.log(`Magic link: ${url}`);
  return;
}
```

The throw prevents the magic link from being logged and surfaces a clear misconfiguration error in production.

## Acceptance criteria

1. `npx tsc --noEmit` exits 0 from `dashboard/`
2. `npx next lint` exits 0 from `dashboard/`
3. No `console.log` call in `dashboard/auth.ts` is reachable when `process.env.NODE_ENV === 'production'` AND `process.env.RESEND_API_KEY` is absent
4. The `console.log` fallback is preserved in the non-production branch (development mode unaffected)
5. `git diff --stat` shows only `dashboard/auth.ts` modified
6. diff is ≤ 10 lines

## Forbidden

- No changes outside `dashboard/auth.ts`
- Do not remove the dev-mode `console.log` — it is intentional for local magic-link testing
- Do not add tests (no test framework configured for Next.js auth in this repo)
- Do not change any other auth behaviour (session, cookie, provider config)
