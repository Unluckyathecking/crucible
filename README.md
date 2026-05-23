# Crucible

[![ci](https://github.com/Unluckyathecking/crucible/actions/workflows/ci.yml/badge.svg)](https://github.com/Unluckyathecking/crucible/actions/workflows/ci.yml)
[![new-tool-smoke](https://github.com/Unluckyathecking/crucible/actions/workflows/new-tool-smoke.yml/badge.svg)](https://github.com/Unluckyathecking/crucible/actions/workflows/new-tool-smoke.yml)
[![go](https://img.shields.io/badge/go-1.25-00ADD8?logo=go)](https://go.dev/)
[![next.js](https://img.shields.io/badge/next.js-15-000000?logo=next.js)](https://nextjs.org/)
[![license](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A clone-and-adapt framework for high-volume metered API products.

Crucible is one repo you copy to ship a new API. The framework — auth, rate limiting, Stripe metered billing, observability, OpenAPI/SDK generation, customer dashboard — is identical across every clone. Per-product logic lives in a single worker process that speaks one frozen contract. Workers can be written in any language that speaks HTTP/JSON.

## Status

v1 shipped. API key auth (salted SHA-256, Redis hot cache), sliding-window rate limiting with atomic Lua scripts, monthly quota enforcement with atomic reserve, Stripe metered billing (async batch flusher, HMAC webhook verification), Prometheus metrics (6 counters/histograms, cardinality capped), health check endpoints, Next.js 15 dashboard (NextAuth magic-link, Resend email, shared Postgres). 16 unit tests passing under `-race`. Pre-release review notes at `docs-internal/REVIEW.md`.

## Layout

| Path | What |
|---|---|
| `gateway/` | Go API gateway. Owns auth, rate limit, billing, metering, OpenAPI, observability. |
| `gateway/proto/tool.proto` | The frozen worker contract. Never edit per product. |
| `gateway/migrations/` | Postgres schema. |
| `workers/sdk-go/` | Shared library every Go worker imports. |
| `workers/stubs/go/` | Hello-world reference worker (~30 lines). |
| `workers/active` | Symlink to the worker this clone is shipping. |
| `docs-internal/` | Design notes and handoffs. Not published. |

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

## Adapting Crucible to a new product

See [ADAPT.md](ADAPT.md). TL;DR: edit `workers/active`, add one route in the gateway, define your plan tiers, and ship.
