# Crucible

[![ci](https://github.com/Unluckyathecking/crucible/actions/workflows/ci.yml/badge.svg)](https://github.com/Unluckyathecking/crucible/actions/workflows/ci.yml)
[![compatibility](https://github.com/Unluckyathecking/crucible/actions/workflows/compatibility.yml/badge.svg)](https://github.com/Unluckyathecking/crucible/actions/workflows/compatibility.yml)
[![new-tool-smoke](https://github.com/Unluckyathecking/crucible/actions/workflows/new-tool-smoke.yml/badge.svg)](https://github.com/Unluckyathecking/crucible/actions/workflows/new-tool-smoke.yml)
[![go](https://img.shields.io/badge/go-1.25-00ADD8?logo=go)](https://go.dev/)
[![next.js](https://img.shields.io/badge/next.js-15-000000?logo=next.js)](https://nextjs.org/)
[![license](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A clone-and-adapt framework for building high-volume, metered API products.

Crucible provides a reusable gateway, customer dashboard, worker contract, generated
clients, billing, rate limiting, and observability. Product-specific logic stays in a
worker process that speaks a small HTTP/JSON contract.

## Project status

Crucible is under active development. The default branch is exercised by unit,
integration, race, vulnerability, conformance, container-build, and clone-acceptance
checks. There is not yet a tagged stable release, so adopters should pin a commit and
review [API stability](docs/api-stability.md) before upgrading.

The repository currently includes:

- a Go gateway with API-key auth, rate limits, quotas, metered billing, OpenAPI,
  health checks, audit events, webhooks, async jobs, metrics, and tracing;
- a Next.js customer and operator dashboard;
- Go, Rust, TypeScript, and Python worker SDKs and conformance fixtures;
- generated Go, TypeScript, and Python consumer clients; and
- reference workers plus a clone-and-adapt acceptance test.

## Quick start

The shortest path exercises a worker without external services.

### Prerequisites

- Git
- Go 1.25.12 (the toolchain declared by the gateway and worker modules)
- `curl`

```bash
git clone https://github.com/Unluckyathecking/crucible.git
cd crucible
make worker
```

In another terminal:

```bash
curl -sS -X POST http://localhost:8081/invoke \
  -H 'content-type: application/json' \
  -d '{"operation":"echo","payload":{"x":"hi"},"metadata":{"units":"3"}}'
```

The response should contain an echoed payload and `"billable_units":3`.

For the complete local stack, install Docker with Compose v2, copy the example
configuration, and start the services:

```bash
cp .env.example .env
make dev
```

The complete gateway requires PostgreSQL and Redis. Stripe and email credentials are
only needed for the corresponding billing and magic-link flows. Never commit `.env`.
See [.env.example](.env.example) for every setting and [ADAPT.md](ADAPT.md) before
turning the template into a product.

## Architecture

```text
consumer -> gateway (:8080) -> active worker (:8081)
                |                    |
                |                    +-- product-specific logic
                +-- PostgreSQL, Redis, Stripe, metrics/traces

browser  -> dashboard (:3000) -> shared PostgreSQL + gateway APIs
```

The gateway owns cross-cutting product infrastructure. `workers/active` selects the
product implementation. `gateway/proto/tool.proto` and the fixture-driven conformance
suite define the language-neutral boundary between them. OpenAPI generated from the
gateway drives the consumer clients.

| Path | Purpose |
|---|---|
| `gateway/` | Go API gateway and database migrations |
| `dashboard/` | Next.js customer/operator application |
| `workers/sdk-*` | Worker SDKs and contract fixtures |
| `workers/stubs/` | Reference workers |
| `clients/` | Generated Go, TypeScript, and Python consumer clients |
| `test/conformance/` | Language-neutral worker contract tests |
| `ops/` and `deploy/` | Example observability and deployment assets |
| `docs/` | Public API, compatibility, stability, and release documentation |
| `docs-internal/` | Development records; not part of the supported public API |

## Development

```bash
make test                 # core Go tests
make smoke-test-new-tool  # clone-and-adapt smoke test
```

The full CI commands and service requirements are documented in
[CONTRIBUTING.md](CONTRIBUTING.md). Supported toolchains and platforms are listed in
[the compatibility matrix](docs/compatibility.md).

## Adapting Crucible

Follow [ADAPT.md](ADAPT.md). In brief: select or create a worker, declare gateway
routes, seed plan tiers, supply product copy and configuration, then run the
conformance and acceptance gates.

## Community and support

- Read [CONTRIBUTING.md](CONTRIBUTING.md) before opening a pull request.
- Use [GitHub Discussions](https://github.com/Unluckyathecking/crucible/discussions)
  when enabled, or [open a support issue](https://github.com/Unluckyathecking/crucible/issues/new/choose)
  according to [SUPPORT.md](SUPPORT.md).
- Report vulnerabilities privately as described in [SECURITY.md](SECURITY.md).
- Participation is governed by the [Code of Conduct](CODE_OF_CONDUCT.md).
- Project decision-making is described in [GOVERNANCE.md](GOVERNANCE.md).

## License

Crucible is licensed under the [MIT License](LICENSE) (`SPDX-License-Identifier: MIT`).
