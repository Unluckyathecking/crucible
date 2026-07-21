# Crucible

[![ci](https://github.com/Unluckyathecking/crucible/actions/workflows/ci.yml/badge.svg)](https://github.com/Unluckyathecking/crucible/actions/workflows/ci.yml)
[![new-tool-smoke](https://github.com/Unluckyathecking/crucible/actions/workflows/new-tool-smoke.yml/badge.svg)](https://github.com/Unluckyathecking/crucible/actions/workflows/new-tool-smoke.yml)
[![go](https://img.shields.io/badge/go-1.25-00ADD8?logo=go)](https://go.dev/)
[![next.js](https://img.shields.io/badge/next.js-15-000000?logo=next.js)](https://nextjs.org/)
[![license](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A clone-and-adapt framework for high-volume metered API products.

Crucible is one repo you copy to ship a new API. The framework — auth, rate limiting, Stripe metered billing, observability, OpenAPI/SDK generation, customer dashboard — is identical across every clone. Per-product logic lives in a single worker process that speaks one frozen contract. Workers can be written in any language that speaks HTTP/JSON.

## Status

v1 shipped and the surface has grown since. The gateway handles API key auth with a Redis hot cache, per-customer rate limits and monthly quotas, Stripe metered billing with HMAC-verified webhooks, request idempotency, async jobs (`/v1/jobs`), outbound webhooks with a deliveries API, self-serve usage and error endpoints, an operator console, Prometheus metrics, and opt-in OpenTelemetry tracing. The dashboard is Next.js 15 with NextAuth magic-link login.

Worker SDKs exist for Go, Rust, TypeScript and Python; all four are conformance-tested in CI against the same frozen fixture. Generated consumer SDKs (Go, TypeScript, Python) live in `clients/`, with a CI drift guard that fails the build if they fall out of sync with the gateway's served OpenAPI document.

## Layout

| Path | What |
|---|---|
| `gateway/` | Go API gateway. Auth, rate limiting, billing, quotas, jobs, webhooks, observability. |
| `gateway/proto/tool.proto` | The frozen worker contract. Never edit per product. |
| `workers/` | Worker SDKs (`sdk-go`, `sdk-rust`, `sdk-ts`, `sdk-python`), language stubs, the conformance fixture, and the `active` symlink pointing at the worker this clone ships. |
| `clients/` | Generated consumer SDKs (Go, TypeScript, Python) and the OpenAPI snapshot they are generated from. |
| `dashboard/` | Next.js customer dashboard: login, API keys, usage. |
| `ops/` | Prometheus and Grafana provisioning. |
| `scripts/` | `new-tool.sh`, the CI smoke test, client generation, dev seeding, `doctor.sh`. |
| `test/` | External conformance suite for the worker contract. |
| `deploy/` | Deploy, backup and host bootstrap scripts. |
| `docs/` | API reference. |

## Running locally

```bash
cp .env.example .env

# In one shell:
make worker     # POST /invoke + /healthz on :8081

# In another:
make gateway    # /healthz on :8080
```

Smoke test the worker directly:

```bash
curl -X POST localhost:8081/invoke \
  -H 'content-type: application/json' \
  -d '{"operation":"echo","payload":{"x":"hi"},"metadata":{"units":"3"}}'
# → {"payload":{"echo":{"x":"hi"},"operation":"echo"},"billable_units":3}
```

To bring up the whole stack instead (Postgres, Redis, worker, gateway, dashboard, Caddy, Prometheus, Grafana), run `make dev`.

## Compatibility

The stable surface is the worker contract. `gateway/proto/tool.proto` is frozen across all clones: the gateway forwards `operation` opaquely, and what your product actually does lives entirely in the worker. The HTTP/JSON form of the contract is pinned by `workers/conformance/fixture.json`, and CI runs every SDK against that fixture on every PR. Everything behind the contract — gateway internals, dashboard, ops — can change between commits; there is no versioned release train.

## Adapting Crucible to a new product

See [ADAPT.md](ADAPT.md). TL;DR: repoint `workers/active`, add one entry to the gateway's route table, seed your plan tiers, and ship.

## Contributing

[CONTRIBUTING.md](CONTRIBUTING.md) covers local setup, what CI runs, and what makes a PR mergeable. The short version: keep CI green and keep the framework product-neutral.

## Security

Report vulnerabilities privately through GitHub's Security tab, not in public issues. Details in [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE).
