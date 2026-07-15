# Contributing to Crucible

Thank you for helping improve Crucible. Contributions of code, tests,
documentation, compatibility reports, and focused design proposals are welcome.

By participating, you agree to follow the [Code of Conduct](CODE_OF_CONDUCT.md).
For help using Crucible, follow [SUPPORT.md](SUPPORT.md). Security reports must use
the private process in [SECURITY.md](SECURITY.md), not a public issue.

## Before you start

Search existing issues and pull requests first. For a substantial feature, public
API change, new dependency, migration, or worker-contract change, open a proposal
issue before investing in an implementation. Maintainers may decline changes that
make the shared framework product-specific or weaken the frozen worker boundary.

Good first contributions are narrowly scoped bug fixes, tests, documentation,
compatibility improvements, or additions to an existing SDK.

## Development setup

The canonical environment uses Go 1.25.12, Node.js 22 with pnpm 10, Python 3.12,
the stable Rust toolchain, PostgreSQL 16, and Redis 7. See
[docs/compatibility.md](docs/compatibility.md) for minimums and CI coverage.

```bash
git clone https://github.com/Unluckyathecking/crucible.git
cd crucible
cp .env.example .env
make test
```

Docker Compose is the easiest way to run PostgreSQL, Redis, the gateway, worker,
and dashboard together:

```bash
make dev
make logs
make down
```

## Making changes

- Keep framework code product-neutral. Product logic belongs in a worker.
- Do not change `gateway/proto/tool.proto` casually. Treat it as a compatibility
  boundary and follow [docs/api-stability.md](docs/api-stability.md).
- Add database migrations as new, lexically ordered, idempotent files. Never edit a
  migration that may already have run.
- Do not hand-edit generated clients. Update the served OpenAPI source/snapshot and
  run `bash scripts/gen-clients.sh`.
- Add or update tests for behavior changes. Avoid secrets, customer data, and
  machine-specific paths in fixtures.
- Update public documentation and `CHANGELOG.md` when behavior users depend on
  changes.

## Verification

Run the checks relevant to your change. CI is the final cross-platform gate.

```bash
make test
bash scripts/smoke-new-tool.sh

cd gateway && go vet ./... && go test -race ./...
cd dashboard && pnpm install --frozen-lockfile && pnpm test && pnpm build
cd workers/sdk-rust && cargo test && cargo clippy -- -D warnings
cd workers/sdk-ts && npm ci && npm test
cd workers/sdk-python && python3 -m unittest conformance.test_fixture_conformance -v
bash scripts/conformance-run.sh go     # also: rust, ts, python
```

Integration and acceptance tests need reachable PostgreSQL and Redis instances; the
GitHub Actions workflows show the canonical environment variables and migration step.

## Pull requests

Create a focused branch and use a descriptive commit and PR title. Complete the pull
request template, link related issues, explain user-facing impact and compatibility,
and include exact verification commands. Keep unrelated refactors in separate PRs.
Maintainers may request changes, additional tests, or a smaller scope.

## Licensing and contributor certification

All repository code and documentation are provided under the MIT License. No CLA or
DCO sign-off is currently required. By submitting a contribution, you represent that
you have the right to submit it and agree that it may be distributed under the
repository's [MIT License](LICENSE). Do not submit third-party material unless its
license is compatible and you include the required attribution.
