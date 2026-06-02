# Workers

A Crucible worker is a single process that implements one frozen contract:

- `POST /invoke` — accepts an `InvokeRequest` JSON, returns an `InvokeResponse` JSON.
- `GET /healthz` — returns 200 OK when ready.

The contract is defined in `gateway/proto/tool.proto`. Workers speak HTTP/JSON by default (curl-debuggable, writable in any language); gRPC is opt-in for perf-sensitive workers.

## Layout

| Path | What |
|---|---|
| `sdk-go/` | Go SDK. Import this, write one function, you have a working worker. |
| `sdk-python/` | (v1.5) |
| `sdk-typescript/` | (v1.5) |
| `sdk-rust/` | Rust SDK. Depend on it, write one async handler, call `serve`. |
| `stubs/` | Hello-world reference impls — one per SDK language. |
| `active` | Symlink to the worker this clone ships. Edit this when adapting Crucible. |

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

## Writing a worker in another language (no SDK yet)

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

## Conformance gate

Every stub in `stubs/` is wired into a language-agnostic contract test suite
(`test/conformance/contract_test.go`) that verifies the frozen HTTP/JSON shapes:

| Stub | Run locally |
|---|---|
| Go | `bash scripts/conformance-run.sh go` |
| Rust | `bash scripts/conformance-run.sh rust` |
| TypeScript | `bash scripts/conformance-run.sh ts` |
| Python | `bash scripts/conformance-run.sh python` |

CI runs all four in a matrix (`.github/workflows/worker-conformance.yml`) with
`fail-fast: false` so every SDK gets a report even if one fails. The suite is
driven by `WORKER_URL` — no language-specific assertion branches exist; the same
tests run against every stub unchanged.

Adding a new language: add a `case` entry in `scripts/conformance-run.sh` and a
matrix entry in `.github/workflows/worker-conformance.yml`. The test file never
changes.

## Billable units

Return `billable_units >= 1` on every successful response.

- Flat-rate tools (one call = one unit): return `1`.
- Per-page parsers: return the number of pages parsed.
- Per-image tools: return the number of images processed.
- Per-token LLM tools: return tokens consumed.

The gateway emits a Stripe `meter_event` with `value=billable_units` for every successful call. Pricing in Stripe is per-unit.
