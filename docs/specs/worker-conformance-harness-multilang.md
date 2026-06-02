# Directive: language-agnostic worker conformance harness (go/rust/ts/python)

**Date:** 2026-06-02
**Type:** 10xworker:job (primary decomposition — workers / framework infrastructure)
**Module:** `worker-conformance-harness-multilang`

See the PR body for the full JSON spec, acceptance criteria, and forbidden list.

## Why

Crucible's entire clone-and-adapt promise rests on one frozen artifact — the
`gateway/proto/tool.proto` HTTP/JSON worker contract. The language-agnostic
conformance suite (`test/conformance/contract_test.go`) and its runner
(`scripts/conformance-run.sh`, `.github/workflows/worker-conformance.yml`) are
written to be extended across languages, but only the `go` stub is wired. The
`rust`/`ts`/`python` stubs ship with **zero** conformance enforcement, and the
Python reference stubs are already non-conformant (healthz body, malformed-body
status, missing `error.retryable`). This unit completes the harness into a true
multi-language contract gate — reusable framework infrastructure that compounds
across every future SDK and every clone.

## Scope (new/edited files only — see PR body globs)

- `test/conformance/**` (additive cases only; never weaken assertions)
- `scripts/conformance-run.sh`, `.github/workflows/worker-conformance.yml`
- `workers/stubs/python/**`, `workers/stubs/rust/Dockerfile`, `workers/README.md`

Disjoint from open PRs #62 (`metrics.go`/`routes.go`), #48 (`main.go`/`auth/store.go`),
#88 (`ci.yml`), #39 (`dashboard/**`), and `clients/**`.
