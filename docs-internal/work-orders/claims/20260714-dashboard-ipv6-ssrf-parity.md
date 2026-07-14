# Claim — dashboard-ipv6-ssrf-parity

**Lane:** `worker:claim` · **Seeded:** 2026-07-14 (cycle 2)

## Directive

**Repo/area:** crucible · `dashboard/app/api/webhooks/route.ts` (webhook-registration URL guard).

**Change:** `isPrivateHostname` (`route.ts:24-53`) rejects IPv4 private/link-local/CGNAT ranges
thoroughly (10/8, 127/8, 169.254/16, 172.16/12, 192.168/16, 0/8, 100.64/10) but for IPv6 only
matches the literal loopback `::1` (`route.ts:31`). Extend it to also reject **IPv6 unique-local
`fc00::/7`** and **IPv6 link-local `fe80::/10`** address literals (handle both bare and
bracketed `[...]` forms, case-insensitively). Add matching test cases to
`dashboard/app/api/webhooks/route.test.ts`, which today has zero IPv6 ULA/link-local coverage
(`grep -riE 'fc00|fe80|ipv6'` → 0 hits).

**Expected outcome:** the dashboard registration path rejects private-IPv6-literal webhook URLs at
registration time, reaching parity with (a) its own IPv4 rigor and (b) the Go gateway's
authoritative `egress.Blocked` guard (`gateway/internal/webhookout/endpoints.go:100`), which already
covers ULA/link-local/mapped at dial time.

**Constraints / respect:**
- Defense-in-depth / consistency only — the authoritative dial-time guard (`egress.Blocked`,
  `gateway/internal/egress/guard.go`) already blocks these, so this is a registration-UX parity fix,
  not a live SSRF hole. Keep it surgical.
- Match the existing string-literal matching style in `isPrivateHostname`; do not pull in a new
  IP-parsing dependency for what is a prefix check on the normalized hostname.
- Do not alter the IPv4 branches or the existing `::1` / `localhost` handling.
- Scope is `dashboard/app/api/webhooks/route.ts` + its test only. Disjoint from #167 (which touches
  `dashboard/lib/license.ts` and `dashboard/app/page.tsx`) and #168 (`.github/**`).
