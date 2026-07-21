# Workers

A Crucible worker is a single process that implements one frozen contract:

- `POST /invoke` — accepts an `InvokeRequest` JSON, returns an `InvokeResponse` JSON.
- `GET /healthz` — returns 200 OK when ready.

The contract is defined in `gateway/proto/tool.proto`. Workers speak HTTP/JSON by default (curl-debuggable, writable in any language); gRPC is opt-in for perf-sensitive workers.

## Layout

| Path | What |
|---|---|
| `sdk-go/` | Go SDK. Import it, write one function, you have a working worker. |
| `sdk-python/` | Python SDK. Standard library only, no third-party dependencies. |
| `sdk-ts/` | TypeScript SDK. Zero dependencies, runs on Node. |
| `sdk-rust/` | Rust SDK. Depend on it, write one async handler, call `serve`. |
| `stubs/` | Reference workers, one per SDK plus textkit. See below. |
| `active` | Symlink to the worker this clone ships (default `stubs/go`). Edit it when adapting. |

## Stubs

Each stub is a complete, runnable worker to copy as a starting point.

| Stub | What it is |
|---|---|
| `stubs/go` | Echo worker on the Go SDK. `active` points here by default. |
| `stubs/textkit` | Go worker with several text operations; the reference for multi-operation products. |
| `stubs/rust` | Echo worker on the Rust SDK. |
| `stubs/ts` | Echo worker on the TypeScript SDK. |
| `stubs/python` | Echo worker in the standard library, no SDK. |

## Writing a Go worker

```go
package main

import (
    "context"
    crucible "github.com/Unluckyathecking/crucible/workers/sdk-go"
)

func main() {
    crucible.Serve(8081, func(ctx context.Context, in crucible.Request) (crucible.Response, error) {
        return crucible.Response{Payload: map[string]string{"hello": "world"}}, nil
    })
}
```

That's a complete worker. The SDK handles server bootstrap, `/healthz`, structured logging, graceful shutdown, request decoding, error sanitisation, and the `billable_units >= 1` default.

## Writing a Rust worker

```rust
use crucible_sdk::{serve, HandlerError, Request, Response};

#[tokio::main]
async fn main() -> Result<(), crucible_sdk::ServeError> {
    serve(8081, |req: Request| async move {
        Ok::<_, HandlerError>(Response::new(serde_json::json!({"hello": "world"})))
    })
    .await
}
```

The Rust SDK (`sdk-rust/`, axum + tokio) mirrors the Go SDK's semantics: it registers
`POST /invoke` + `GET /healthz`, decodes the request, defaults `billable_units` to 1,
returns a `BAD_REQUEST` envelope on malformed bodies, surfaces a handler's `WorkerError`
verbatim, and sanitises any other error to a generic `INTERNAL` envelope (the cause is
logged, never surfaced). Error envelopes are returned with HTTP 200 — the gateway reads
the response *shape* (`payload` vs `error`), not the status. See `stubs/rust/` for the
hello-world echo worker.

## Writing a worker in another language

Speak HTTP/JSON against the contract. The on-wire shapes:

```json
// POST /invoke request
{
  "request_id":  "req_abc",
  "customer_id": "cus_xyz",
  "operation":   "validate_vat",
  "payload":     {"vat_number": "GB123456789"},
  "plan":        "pro",
  "metadata":    {}
}

// Success response
{
  "payload":        {"valid": true},
  "billable_units": 1,
  "units_label":    ""
}

// Error response
{
  "error": {
    "code":      "INVALID_VAT_FORMAT",
    "message":   "VAT number format not recognised",
    "retryable": false
  }
}
```

`/healthz` just needs to return HTTP 200 when the process is ready to serve.

## Conformance

Two layers hold every worker to the frozen contract.

**Fixture-driven, per SDK.** `workers/conformance/fixture.json` is the language-neutral
spec; each SDK loads it and asserts the cases in-process (`sdk-go/conformance`,
`sdk-rust/conformance`, `sdk-ts/conformance`, `sdk-python/conformance`). The
`fixture-conformance` matrix in `.github/workflows/worker-conformance.yml` runs all four
with `fail-fast: false`, so every SDK reports even when one fails. The TS leg also runs
`npm test`, the SDK's unit suite, which includes the HMAC signature matrix.

**External, against a live stub.** `test/conformance/contract_test.go` is driven by
`WORKER_URL` and speaks plain HTTP/JSON, so the same tests run against any stub:

```sh
bash scripts/conformance-run.sh go|rust|ts|python
```

CI runs this layer against the Python stub, which has no SDK, in the
`python-stub-conformance` job, next to the stub's own pytest.

Adding a language: add a `case` in `scripts/conformance-run.sh`, plus a matrix entry in
the workflow for a fixture suite. The fixture and the external test file stay the same.

## Billable units

Return `billable_units >= 1` on every successful response.

- Flat-rate tools (one call = one unit): return `1`.
- Per-page parsers: return the number of pages parsed.
- Per-image tools: return the number of images processed.
- Per-token LLM tools: return tokens consumed.

The gateway emits a Stripe `meter_event` with `value=billable_units` for every successful call. Pricing in Stripe is per-unit.

## Cloning

`scripts/new-tool.sh` renames Go module paths and `crucible` identifiers to your product
name across the Go, TypeScript, and config files it copies. The Rust and Python SDK
internals keep their original names (the `crucible_sdk` crate, the `crucible` package):
they are imported by fixed names, so renaming them per clone would break those imports
with no benefit.
