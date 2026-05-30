# Directive: ci-govulncheck

**Worker:** worker:claim (small, direct implementation)
**Branch:** `ultra-plan/20260529-ci-govulncheck`
**Date:** 2026-05-29

## Target

- **Repo:** `crucible`
- **Area:** `.github/workflows/ci.yml` (CI only)

## Change to make

Add a `govulncheck` job to the `ci` workflow that scans the repo's Go modules for
known vulnerabilities (CVEs in dependencies and the Go stdlib). Today `ci.yml`
runs only `go vet` + `go test -race` on the Go modules and has no vulnerability
scanning of any kind.

Implement as a **new, independent job** (so it does not slow the existing `go`
job) that:

1. Checks out and sets up Go `1.25` (match the existing `go` job; reuse module
   caching as that job does).
2. Installs govulncheck (`go install golang.org/x/vuln/cmd/govulncheck@latest`).
3. Runs `govulncheck ./...` in each Go module: `gateway/`, `workers/sdk-go/`, and
   `workers/stubs/go/`.

## Expected outcome

CI fails when any in-use Go dependency or stdlib symbol has a known vulnerability,
catching supply-chain issues before merge. A clean tree passes.

## Constraints

- Modify **only** `.github/workflows/ci.yml`. No Go source, proto, or dashboard
  changes.
- Do not alter the behaviour of the existing `go`, `rust`, or `dashboard` jobs.
- Use Go `1.25` to match `gateway/go.mod` / the existing `go` job.
- Run `govulncheck` against all three Go modules so none is left unscanned.
- Keep it a required, blocking job (consistent with the repo's other CI gates).
