//! HTTP server for Crucible Rust workers.
//!
//! Mirrors the Go SDK at `workers/sdk-go/crucible.go`. A worker is an HTTP server
//! with two endpoints:
//!
//! - `POST /invoke`  — decodes a [`Request`], runs the handler, returns the result.
//! - `GET  /healthz` — returns 200 OK when ready.
//!
//! The on-wire contract is frozen (`gateway/proto/tool.proto`): the gateway forwards
//! `operation` opaquely, and every successful response MUST carry `billable_units >= 1`
//! (invariant #2). The gateway distinguishes success from failure by response *shape*
//! (`payload` vs `error`), not HTTP status — so handler errors return an `error` envelope
//! with HTTP 200, exactly like the Go SDK.

use std::error::Error as StdError;
use std::fmt;
use std::sync::Arc;

use axum::{
    extract::State,
    http::StatusCode,
    response::{IntoResponse, Response as AxumResponse},
    routing::{get, post},
    Json, Router,
};
use serde::{Deserialize, Serialize};

pub type ServeError = Box<dyn StdError + Send + Sync>;

/// Request mirrors the `InvokeRequest` proto for handlers that don't depend on
/// generated proto types. Field names match the Go SDK's JSON tags exactly.
#[derive(Debug, Clone, Deserialize)]
pub struct Request {
    #[serde(default)]
    pub request_id: String,
    #[serde(default)]
    pub customer_id: String,
    #[serde(default)]
    pub operation: String,
    #[serde(default)]
    pub payload: serde_json::Value,
    #[serde(default)]
    pub plan: String,
    #[serde(default)]
    pub metadata: std::collections::HashMap<String, String>,
}

/// Response is what a handler returns on success. `billable_units` defaults to 1
/// when the handler leaves it at 0 — never serialise < 1 (invariant #2).
#[derive(Debug, Clone, Serialize)]
pub struct Response {
    pub payload: serde_json::Value,
    pub billable_units: u64,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub units_label: String,
}

impl Response {
    /// Construct a successful response with `billable_units = 1`.
    pub fn new(payload: serde_json::Value) -> Self {
        Self {
            payload,
            billable_units: 1,
            units_label: String::new(),
        }
    }

    /// Set the billable units (must be >= 1; 0 is normalised to 1 on serialise).
    pub fn with_units(mut self, units: u64) -> Self {
        self.billable_units = units;
        self
    }

    /// Set the optional units label.
    pub fn with_units_label(mut self, label: impl Into<String>) -> Self {
        self.units_label = label.into();
        self
    }
}

/// A structured error a handler can return to surface a stable code to the customer.
/// Mirrors the Go SDK's `Error`. Handlers may also return any other error type, which
/// the SDK wraps as a generic `INTERNAL` error (the real cause is logged, never surfaced).
#[derive(Debug, Clone, Serialize)]
pub struct WorkerError {
    pub code: String,
    pub message: String,
    #[serde(default)]
    pub retryable: bool,
}

impl WorkerError {
    pub fn new(code: impl Into<String>, message: impl Into<String>) -> Self {
        Self {
            code: code.into(),
            message: message.into(),
            retryable: false,
        }
    }

    pub fn retryable(mut self, retryable: bool) -> Self {
        self.retryable = retryable;
        self
    }
}

impl fmt::Display for WorkerError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}: {}", self.code, self.message)
    }
}

impl StdError for WorkerError {}

/// HandlerError is what handlers return on failure. A [`WorkerError`] is surfaced
/// verbatim; any other error is sanitised to a generic internal error envelope.
pub enum HandlerError {
    /// Surface this exact code/message to the customer.
    Worker(WorkerError),
    /// Sanitise to `{code: "INTERNAL", message: "internal error", retryable: true}`;
    /// the wrapped cause is logged server-side only.
    Internal(ServeError),
}

impl HandlerError {
    /// Wrap an opaque error as a sanitised `INTERNAL` failure. The cause is logged
    /// server-side only; the customer sees a generic envelope.
    pub fn internal(cause: impl Into<ServeError>) -> Self {
        HandlerError::Internal(cause.into())
    }
}

impl From<WorkerError> for HandlerError {
    fn from(e: WorkerError) -> Self {
        HandlerError::Worker(e)
    }
}

/// The worker's single entry point. Mirrors the Go SDK's `HandlerFunc`.
#[async_trait::async_trait]
pub trait Handler: Send + Sync + 'static {
    async fn handle(&self, req: Request) -> Result<Response, HandlerError>;
}

#[async_trait::async_trait]
impl<F, Fut> Handler for F
where
    F: Fn(Request) -> Fut + Send + Sync + 'static,
    Fut: std::future::Future<Output = Result<Response, HandlerError>> + Send,
{
    async fn handle(&self, req: Request) -> Result<Response, HandlerError> {
        self(req).await
    }
}

/// Build the worker router. Exposed (separately from [`serve`]) so it can be driven
/// directly in tests without binding a TCP socket.
pub fn router(handler: impl Handler) -> Router {
    let state: Arc<dyn Handler> = Arc::new(handler);
    Router::new()
        .route("/healthz", get(health_handler))
        .route("/invoke", post(invoke_handler))
        .with_state(state)
}

/// Run the worker HTTP server on the given port and block until the listener fails.
pub async fn serve(port: u16, handler: impl Handler) -> Result<(), ServeError> {
    let app = router(handler);
    let listener = tokio::net::TcpListener::bind(format!("0.0.0.0:{port}")).await?;
    tracing::info!(port, "worker listening");
    axum::serve(listener, app).await?;
    Ok(())
}

async fn health_handler() -> impl IntoResponse {
    (StatusCode::OK, Json(serde_json::json!({"status": "ok"})))
}

async fn invoke_handler(
    State(handler): State<Arc<dyn Handler>>,
    body: axum::body::Bytes,
) -> AxumResponse {
    // Decode manually so a malformed body yields the SDK's BAD_REQUEST envelope
    // rather than axum's default plain-text 400 — matching the Go SDK.
    let req: Request = match serde_json::from_slice(&body) {
        Ok(req) => req,
        Err(_) => {
            return error_envelope(WorkerError::new("BAD_REQUEST", "invalid request body"));
        }
    };

    let request_id = req.request_id.clone();
    let operation = req.operation.clone();

    match handler.handle(req).await {
        Ok(mut resp) => {
            if resp.billable_units == 0 {
                resp.billable_units = 1;
            }
            (StatusCode::OK, Json(resp)).into_response()
        }
        Err(HandlerError::Worker(werr)) => {
            tracing::info!(
                request_id = %request_id,
                operation = %operation,
                code = %werr.code,
                "handler returned structured error"
            );
            error_envelope(werr)
        }
        Err(HandlerError::Internal(cause)) => {
            tracing::error!(
                request_id = %request_id,
                operation = %operation,
                error = %cause,
                "handler failed"
            );
            error_envelope(WorkerError::new("INTERNAL", "internal error").retryable(true))
        }
    }
}

/// Returns HTTP 200 with an `{ "error": { ... } }` envelope. The gateway distinguishes
/// success vs error by the response shape, not the HTTP status — matching the proto's
/// `oneof result { payload | error }` semantics and the Go SDK's `writeStructuredError`.
fn error_envelope(err: WorkerError) -> AxumResponse {
    (StatusCode::OK, Json(serde_json::json!({ "error": err }))).into_response()
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::body::{to_bytes, Body};
    use axum::http::{Request as HttpRequest, StatusCode};
    use tower::ServiceExt; // for `oneshot`

    fn echo_router() -> Router {
        router(|req: Request| async move {
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
        })
    }

    async fn body_json(resp: AxumResponse) -> serde_json::Value {
        let bytes = to_bytes(resp.into_body(), usize::MAX).await.unwrap();
        serde_json::from_slice(&bytes).unwrap()
    }

    #[tokio::test]
    async fn healthz_returns_200() {
        let resp = echo_router()
            .oneshot(
                HttpRequest::builder()
                    .uri("/healthz")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(resp.status(), StatusCode::OK);
        assert_eq!(body_json(resp).await, serde_json::json!({"status": "ok"}));
    }

    #[tokio::test]
    async fn invoke_echoes_payload_and_defaults_units_to_one() {
        let resp = echo_router()
            .oneshot(
                HttpRequest::builder()
                    .method("POST")
                    .uri("/invoke")
                    .header("content-type", "application/json")
                    .body(Body::from(
                        r#"{"operation":"echo","payload":{"x":"hi"}}"#,
                    ))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(resp.status(), StatusCode::OK);
        let json = body_json(resp).await;
        assert_eq!(json["payload"]["echo"]["x"], "hi");
        assert_eq!(json["payload"]["operation"], "echo");
        assert_eq!(json["billable_units"], 1);
    }

    #[tokio::test]
    async fn invoke_honours_billable_units_from_metadata() {
        let resp = echo_router()
            .oneshot(
                HttpRequest::builder()
                    .method("POST")
                    .uri("/invoke")
                    .body(Body::from(
                        r#"{"operation":"echo","payload":{},"metadata":{"units":"5"}}"#,
                    ))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(body_json(resp).await["billable_units"], 5);
    }

    #[tokio::test]
    async fn malformed_body_returns_bad_request_envelope() {
        let resp = echo_router()
            .oneshot(
                HttpRequest::builder()
                    .method("POST")
                    .uri("/invoke")
                    .body(Body::from("{not json"))
                    .unwrap(),
            )
            .await
            .unwrap();
        // HTTP 200 with an error envelope — the gateway reads shape, not status.
        assert_eq!(resp.status(), StatusCode::OK);
        let json = body_json(resp).await;
        assert_eq!(json["error"]["code"], "BAD_REQUEST");
    }

    #[tokio::test]
    async fn handler_worker_error_is_surfaced_verbatim() {
        let app = router(|_req: Request| async move {
            Err(HandlerError::from(WorkerError::new(
                "INVALID_VAT_FORMAT",
                "VAT number format not recognised",
            )))
        });
        let resp = app
            .oneshot(
                HttpRequest::builder()
                    .method("POST")
                    .uri("/invoke")
                    .body(Body::from(r#"{"operation":"x","payload":{}}"#))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(resp.status(), StatusCode::OK);
        let json = body_json(resp).await;
        assert_eq!(json["error"]["code"], "INVALID_VAT_FORMAT");
        assert_eq!(json["error"]["retryable"], false);
    }

    #[tokio::test]
    async fn handler_opaque_error_is_sanitised_to_internal() {
        let app = router(|_req: Request| async move {
            // A plain std error — must NOT leak its message to the customer.
            let cause = std::io::Error::other("secret db dsn leaked here");
            Err::<Response, HandlerError>(HandlerError::internal(cause))
        });
        let resp = app
            .oneshot(
                HttpRequest::builder()
                    .method("POST")
                    .uri("/invoke")
                    .body(Body::from(r#"{"operation":"x","payload":{}}"#))
                    .unwrap(),
            )
            .await
            .unwrap();
        let json = body_json(resp).await;
        assert_eq!(json["error"]["code"], "INTERNAL");
        assert_eq!(json["error"]["message"], "internal error");
        assert_eq!(json["error"]["retryable"], true);
        // The opaque cause must not be in the envelope.
        assert!(!json.to_string().contains("secret db dsn"));
    }

    #[tokio::test]
    async fn zero_billable_units_is_normalised_to_one() {
        let app = router(|_req: Request| async move {
            Ok(Response::new(serde_json::json!({})).with_units(0))
        });
        let resp = app
            .oneshot(
                HttpRequest::builder()
                    .method("POST")
                    .uri("/invoke")
                    .body(Body::from(r#"{"operation":"x","payload":{}}"#))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(body_json(resp).await["billable_units"], 1);
    }
}
