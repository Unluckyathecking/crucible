//! Hello-world Crucible worker stub (Rust).
//!
//! Every Rust worker in a Crucible clone starts from this shape: depend on the SDK,
//! implement one handler, call `serve`. Per-product logic lives entirely in the
//! handler body.
//!
//! Run locally:  cargo run
//!
//! Smoke test:
//!
//!   curl -X POST localhost:8081/invoke \
//!     -H 'content-type: application/json' \
//!     -d '{"operation":"echo","payload":{"x":"hi"},"metadata":{"units":"3"}}'

use crucible_sdk::{serve, HandlerError, Request, Response};

#[tokio::main]
async fn main() -> Result<(), crucible_sdk::ServeError> {
    let port = std::env::var("PORT")
        .ok()
        .and_then(|v| v.parse::<u16>().ok())
        .unwrap_or(8081);
    serve(port, handle).await
}

/// Echoes the request payload back. If `metadata["units"]` is a positive integer,
/// it is returned as `billable_units` — useful for testing per-unit billing end-to-end.
async fn handle(req: Request) -> Result<Response, HandlerError> {
    let units = req
        .metadata
        .get("units")
        .and_then(|v| v.parse::<u64>().ok())
        .filter(|n| *n >= 1)
        .unwrap_or(1);

    Ok(Response::new(serde_json::json!({
        "echo": req.payload,
        "operation": req.operation,
    }))
    .with_units(units))
}
