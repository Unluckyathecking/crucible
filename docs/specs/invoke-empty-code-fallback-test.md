# Claim — `invoke-empty-code-fallback-test`

`worker:claim` — small, test-only.

## Target

`gateway/internal/server/routes_test.go` (test addition only; no production code).

## Gap (verified against `main`, HEAD f1e6331)

`internal/server/routes.go:248-252`: when `WORKER_ERROR_EXPOSURE=full` and a
non-SDK worker returns an error with an **empty** `error.code`, the `invoke`
handler substitutes `apierror.WORKER_BAD_RESPONSE` so a customer never receives
`"code":""` (an empty code is non-correlatable). The existing
`TestInvokeErrorExposureFull` (`routes_test.go:284`) only exercises a **non-empty**
code (`INVALID_INPUT`). Grep confirms no test drives full-mode with an empty code,
so this security-relevant fallback branch has zero coverage.

## Change

Add a test that, with `WORKER_ERROR_EXPOSURE=full`, drives a stub worker which
returns a successful-transport response carrying `error.code == ""` and asserts
the gateway responds `502` with body `code == "WORKER_BAD_RESPONSE"` (and a safe,
non-empty message). Follow the existing `TestInvokeErrorExposureFull` harness
style (same worker stub + recorder pattern).

## Expected outcome

A new passing test pinning the empty-code → `WORKER_BAD_RESPONSE` substitution;
`go test -race ./internal/server/...` green.

## Constraints

- Test-only: do **not** modify `routes.go` or any production code.
- Do not touch the `billable_units < 1` → 502 trust-boundary check or its tests.
- Disjoint from PR #112 (`openapi-route-registry`, which changes route *mounting*,
  not the `invoke` handler body) and from PR #48 (main.go/store.go).
- Reuse real Postgres/Redis per the no-mock rule if the harness requires them;
  match the existing error-exposure test's setup.
