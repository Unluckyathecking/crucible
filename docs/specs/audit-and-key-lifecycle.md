# Spec: audit-and-key-lifecycle (10xworker:job)

Wire up the **dormant `audit_log` table** (defined in `gateway/migrations/0001_init.sql`,
zero writers today) behind one shared, reusable audit emitter, and complete the
**API key lifecycle** by adding self-service revocation (create exists; revoke is
unreachable dead code — `auth.Store.Revoke` has no non-test callers, and the
dashboard has no DELETE/revoke route).

Full module spec, scope globs, acceptance criteria, forbidden constraints, and the
sub-unit decomposition are in the PR description (fenced JSON block).

Module slug: `audit-and-key-lifecycle`.
