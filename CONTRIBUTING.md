# Contributing

Crucible is a template repo with a single maintainer. Contributions are welcome; the bar is that CI stays green and the framework stays product-neutral.

## Running it locally

You need Go 1.25+, and Node 22 with pnpm if you're touching the dashboard. A Rust toolchain is optional: the smoke test runs a `cargo check` on the Rust SDK when cargo is installed and skips it otherwise (CI always runs it). Config comes from `.env`, which you create once:

```bash
cp .env.example .env
```

The whole stack runs under docker compose:

```bash
make dev    # postgres, redis, worker, gateway, dashboard, caddy, prometheus, grafana
```

Or run the pieces directly against your own Postgres and Redis (brew services are fine; set `POSTGRES_DSN` and `REDIS_URL` in `.env`):

```bash
make worker     # stub worker on :8081
make gateway    # gateway on :8080; migrations run on boot
```

`make help` lists the rest. Two scripts worth knowing: `scripts/doctor.sh` checks the invariants that break silently (env parity between gateway and dashboard, the `workers/active` symlink, the route table), and `scripts/seed-dev.sh` creates a dev customer and API key so you can make authenticated calls.

## What CI runs

Four workflows gate changes. The first three run on every PR; the drift guard fires only when the client-facing surface changes (`clients/`, the OpenAPI builder, the route files). Reproduce the relevant ones locally before pushing and you'll rarely be surprised:

- `ci` — `go vet` and `go test -race` for the gateway (unit and integration, against real Postgres and Redis), the Go worker SDK tests, a worker stub build, `docker compose build`, govulncheck, the Rust SDK (check, clippy, test), and the dashboard (`pnpm test`, then `pnpm build`).
- `new-tool-smoke` — one job runs `bash scripts/smoke-new-tool.sh`; a second clones a demo tree with `scripts/new-tool.sh` and runs the acceptance suite against it end to end.
- `worker-conformance` — every worker SDK (Go, Rust, TypeScript, Python) tested against `workers/conformance/fixture.json`, plus the Python stub against the suite in `test/conformance/`.
- `Client SDK drift guard` — checks that `clients/openapi.json` matches the OpenAPI document the gateway actually serves, regenerates the consumer SDKs from it, and fails if anything checked in has drifted. Then builds and tests all three clients.

The local equivalents: `go test -race ./...` in `gateway/` and `workers/sdk-go/`; `bash scripts/smoke-new-tool.sh` after touching `scripts/`, env templates, Dockerfiles or `go.mod`; `pnpm test` and `pnpm build` in `dashboard/` after any TypeScript change; `bash scripts/gen-clients.sh` after changing routes or the OpenAPI builder.

## Style

Match the code around you. Dependency choices are deliberately boring: chi, pgx, zerolog, the things already in `go.mod`. A new dependency needs a stated reason in the PR. Comments explain why, not what; the two-phase explanation at the top of `gateway/internal/usage/flusher.go` is the house standard. Tests run against real Postgres and Redis, not mocks — mocks let migrations and queries drift apart silently.

## Pull requests

Small and single-purpose beats large and mixed. Behavior changes come with tests. Commit messages say what changed and why, in plain language. Anything touching billing, auth or webhook code paths gets a slower, line-by-line review; plan for that.

## Framework vs product

Changes to this repo should make sense for every product built on it. Product-specific work belongs in a clone, where it lives in a few known places: the `workers/active` symlink, one entry per endpoint in `gateway/internal/server/routes_table.go`, and a plan seed migration at the next free index in `gateway/migrations/`, plus dashboard copy and Stripe config. [ADAPT.md](ADAPT.md) walks through all of it. If your change only makes sense for one product, it belongs in that product's clone, not here.
