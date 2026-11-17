# Restructure plan

Status: done. Written 2026-11-16 against main after #224; PRs #226 to #233 merged the same day.

## Goal

Make the repo easier to read and change without altering what it does. Delete
and consolidate first; add nothing speculative. Every step is a small PR that
merges on green required checks (go, rust, dashboard, smoke, acceptance).

## What the survey found

The top-level layout is sound. The import graph of `gateway/internal` has no
cycles, nothing below `server` imports `server` or `runtime`, and no package or
script is orphaned. The problems are inside the tree:

- God files. `openapi/openapi.go` (1508 lines), `jobs/store.go` (804),
  `webhookout/emitter.go` (766) and `validate/validate.go` (645) each hold
  several separable responsibilities. `scripts/gen-clients.sh` is 2621 lines,
  of which about 2570 are one Python program inside a heredoc.
- Copy-paste. The date-range parsing in `selferrors` and `selfusagedetail` is
  verbatim (about 60 lines each, same error strings). `operator` and
  `webhookout` carry byte-identical `writeJSON` helpers. The request-id context
  lookup is spelled out 44 times across 17 files with no accessor.
  `openapi.Handler` repeats the same path-item merge loop five times.
- Drifted docs. README and CONTRIBUTING say Go 1.25 (it is 1.26). AGENTS.md and
  ADAPT.md give 0026 as the next migration index (it is 0027). AGENTS.md's file
  map misses `internal/testdb` and `gateway/test/*`. `workers/README.md` says
  gRPC is opt-in; nothing serves or consumes it. `.env.example` lacks
  `WEBHOOK_ENDPOINT_FAILURE_THRESHOLD`. `docs/api-reference.md` lacks
  `POST /v1/webhooks/endpoints/{id}/enable` and `GET /openapi.json`.
- Stale files. `docs/sprint-plans/` holds one plan for work that merged in
  #202. `docs-internal/full-code-review-2026-05-16.md` lists as open six
  findings that `REVIEW.md` records as fixed, and embeds a local path.
  `docs-internal/vies-research.md` is research for a worker that is not in
  this repo.
- CI gaps. `workers/sdk-python/tests/test_signature.py` is never run in CI.
  `workers/stubs/rust` is never compiled in CI.

## Target layout

Top-level directories and every Go import path stay as they are. Changes:

```
docs/                      api-reference.md only (sprint-plans/ removed)
docs-internal/             REVIEW.md, restructure-plan.md
scripts/gen-clients.sh     thin wrapper (repo root, python3, gofmt)
scripts/gen_clients.py     the generator, moved verbatim out of the heredoc
gateway/internal/
  openapi/    types.go, helpers.go, webhooks.go, build.go, handler.go,
              paths_*.go (one per resource)
  validate/   validate.go, numeric.go (+ existing middleware.go)
  jobs/       store.go, store_claim.go, store_transitions.go
  webhookout/ emitter.go, emitter_claim.go, emitter_deliver.go
  middleware/ + RequestIDFrom(ctx)
  httputil/   + WriteJSON, ParseDateRange
```

## PRs, in merge order

Branch protection requires branches to be up to date, so merges are serial
even where the work is parallel. PRs 2, 3, 4, 6 and 7 own disjoint paths and
are written concurrently; PR 5 starts after 3 and 4 merge.

Merged as: 1 #226, 2 #227, 3 #230, 4 #229, 5 #233, 6 #228, 7 #231. One PR was
added during the work: #232 removed home-directory paths, a private repo name
and a stale local-first rule from AGENTS.md, and fixed the bootstrap script's
placeholder URL and the `server` package comment.

| # | Title | Paths owned | Risk | Tier |
|---|---|---|---|---|
| 1 | docs: add restructure plan | `docs-internal/restructure-plan.md` | none | lead |
| 2 | docs: remove stale internal docs and correct drifted facts | `README.md`, `CONTRIBUTING.md`, `ADAPT.md`, `AGENTS.md` (facts only), `workers/README.md`, `docs/`, `docs-internal/` except this file, `.gitignore`, `.env.example` | low; no code | Sonnet |
| 3 | refactor: split openapi.go and validate.go by responsibility | non-test files in `gateway/internal/openapi/`, `gateway/internal/validate/validate.go` and new siblings | low; declarations move between files in the same package, nothing else | Sonnet |
| 4 | refactor: split the jobs store and webhook emitter by responsibility | `gateway/internal/jobs/store*.go`, `gateway/internal/webhookout/emitter*.go` | low; same as 3. SQL strings must stay byte-identical | Sonnet |
| 5 | refactor: share request-id, JSON and date-range helpers | `gateway/internal/middleware`, `httputil`, `selferrors`, `selfusagedetail`, `operator/handlers.go`, `webhookout/adminhttp.go`, `openapi/handler.go`, plus the one-line request-id call sites | medium; touches handlers. Headers, status codes and error strings must not change | Sonnet, lead reviews every hunk |
| 6 | refactor: move the client generator out of its shell heredoc | `scripts/gen-clients.sh`, `scripts/gen_clients.py`, `.github/workflows/client-sdk-drift.yml`, `clients/README.md` | low; drift workflow proves the output is byte-identical | Sonnet |
| 7 | ci: run the python sdk tests and compile the rust stub | `.github/workflows/worker-conformance.yml`, `.github/workflows/ci.yml` (rust job only) | low; may surface an existing failure, in which case stop and report | Haiku |

## How behaviour preservation is verified

- Required checks on every PR: gateway unit tests with `-race`, the
  integration suite under `gateway/test/` (acceptance, scenarios, textkit),
  sdk-go tests, compose build, rust, dashboard, new-tool smoke and acceptance.
- `client-sdk-drift` diffs `spec-dump` output against `clients/openapi.json`
  byte for byte and regenerates the three clients with `git diff --exit-code`.
  It guards PRs 3, 5 (openapi) and 6 (generator).
- `worker-conformance` runs the fixture suite for four SDKs and the HTTP
  contract suite in `test/conformance` against the Python stub.
- PRs 3 and 4 are pure moves. The reviewer checks that the sorted multiset of
  non-import, non-package lines in each package's non-test files is identical
  before and after, and that no `_test.go` file changed.
- PR 5 changes no test expectations. Existing handler tests pin the error
  strings and status codes.
- PR 6: the body of `gen_clients.py` equals the old heredoc body.
- No PR edits a migration, `proto/tool.proto`, `routes_table.go`, a metric
  name, a Redis key, or a test assertion.

## Out of scope

- Moving or renaming Go packages (`cache`, the three `self*` packages,
  `egress`, `channelsig`, `events`). `auth`, `billing`, `ratelimit`, `quota`,
  `proxy` and `observability` are the framework surface that product clones
  must not edit; import-path churn there costs every clone a merge for no
  behaviour gain.
- Merging the `captureWriter` in `idempotency` and `respcache`. The code says
  the copy is deliberate.
- Collapsing the duplicated metric definitions in `observability/metrics.go`
  (`NewMetricsForTest`). Names, help text and buckets are a scrape contract and
  the test copy has already drifted in field names; needs its own careful PR.
- Unifying the three retry/backoff mechanisms, the two fair-claim
  implementations, or the three UTF-8 truncation routines. They differ in
  behaviour on purpose or by accident, and unifying them changes behaviour.
- Separating HTTP from SQL in `billing/webhook.go` and `auth`. The webhook
  ordering invariant lives there.
- Splitting `dashboard/lib/db.ts` (728 lines). Go and dashboard tests read that
  file by path as source text; splitting it means rewriting the tests that
  prove parity.
- The duplicated `jobs.Store` construction in `main` and `NewRouter`, and the
  harness emitter that outlives its test server. Real, but fixing them changes
  wiring behaviour.
- The operating-standard block at the top of AGENTS.md. Facts about the code
  and machine-specific references were corrected; what general guidance the
  file should carry is the owner's call.
- `seed-dev.sh` and the three `deploy/` scripts are not exercised by CI.
- ~~`govulncheck` is installed at `@latest`; `clients/go` is in the `ci` path
  filter but only the drift workflow builds it.~~ Done: govulncheck is pinned
  to v1.1.4 and `clients/go` is out of the `ci` filter (the drift workflow
  builds and tests it).

## Needs an owner decision (not done here)

- `docker-compose.yml` publishes Postgres 5432 and Redis 6379 on all
  interfaces. Docker bypasses ufw for published ports, so the firewall set up
  by `deploy/hetzner-bootstrap.sh` does not cover them.
- ~~Grafana alert rules reference `datasourceUid: prometheus` but the
  provisioned datasource sets no `uid`.~~ Done: the datasource now sets
  `uid: prometheus`.
- `workers/sdk-python` has no consumer; `stubs/python` reimplements the
  contract with the standard library.
- `gateway/proto/tool.proto` is the frozen contract but nothing generates code
  from it. Done: `proxy/proto_contract_test.go` now checks its field names
  against the JSON structs in `proxy/client.go`.
- ~~The `rust`, `ts` and `textkit` stub Dockerfiles are never built in CI.~~
  Done: the `go` job builds them.
- ~~`workers/stubs/rust/Cargo.lock` is out of date with `Cargo.toml`.~~ Done:
  regenerated, and the CI step runs with `--locked` again.
