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
    body::to_bytes,
    extract::State,
    http::StatusCode,
    response::{IntoResponse, Response as AxumResponse},
    routing::{get, post},
    Json, Router,
};
use hmac::{Hmac, Mac};
use sha2::Sha256;
use serde::{Deserialize, Serialize};

pub type ServeError = Box<dyn StdError + Send + Sync>;

type HmacSha256 = Hmac<Sha256>;

/// Header carrying the inbound channel-auth signature.
/// Format: t=<unix-seconds>,v1=<hex-hmac-sha256>
/// Signing payload: HMAC-SHA256(secret, timestamp + "." + body) — byte-identical
/// to the Go SDK and TS SDK so cross-language parity is maintained.
const WORKER_SIG_HEADER: &str = "x-worker-signature";

/// Maximum age (or future skew) of a signed request in seconds.
/// Mirrors the 5-minute replay window used for Stripe webhooks.
const WORKER_SIG_WINDOW: i64 = 300;

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

/// Configuration for the worker HTTP handler.
/// The zero value disables all optional features, preserving today's behaviour.
#[derive(Default, Clone)]
pub struct HandlerConfig {
    /// HMAC-SHA256 key for inbound /invoke request verification.
    /// `None` disables verification (today's behaviour). When [`router`] is called
    /// directly, WORKER_SHARED_SECRET from the environment is used automatically.
    pub shared_secret: Option<Vec<u8>>,
}

/// Internal router state shared across requests via Arc.
#[derive(Clone)]
struct AppState {
    handler: Arc<dyn Handler>,
    secret: Option<Vec<u8>>,
}

/// Build the worker router with default configuration.
/// When WORKER_SHARED_SECRET is set in the environment, inbound /invoke
/// requests are verified before the handler is called.
pub fn router(handler: impl Handler) -> Router {
    let secret = std::env::var("WORKER_SHARED_SECRET")
        .ok()
        .filter(|s| !s.is_empty())
        .map(|s| s.into_bytes());
    router_with_config(handler, HandlerConfig { shared_secret: secret })
}

/// Build the worker router with explicit configuration.
/// Use this in tests or when configuring the secret programmatically rather than
/// via the WORKER_SHARED_SECRET environment variable.
pub fn router_with_config(handler: impl Handler, config: HandlerConfig) -> Router {
    let state = Arc::new(AppState {
        handler: Arc::new(handler),
        secret: config.shared_secret,
    });
    Router::new()
        .route("/healthz", get(health_handler))
        .route("/invoke", post(invoke_handler))
        .with_state(state)
}

/// Run the worker HTTP server on the given port and block until SIGINT/SIGTERM,
/// then drain in-flight requests before the listener closes.
pub async fn serve(port: u16, handler: impl Handler) -> Result<(), ServeError> {
    let app = router(handler);
    let listener = tokio::net::TcpListener::bind(format!("0.0.0.0:{port}")).await?;
    tracing::info!(port, "worker listening");
    axum::serve(listener, app)
        .with_graceful_shutdown(shutdown_signal())
        .await?;
    Ok(())
}

/// Resolves when the process receives SIGINT or SIGTERM, so axum can drain
/// in-flight requests before shutting down — matching the Go/Python/TS SDKs.
async fn shutdown_signal() {
    let ctrl_c = async {
        tokio::signal::ctrl_c()
            .await
            .expect("install Ctrl+C handler");
    };

    #[cfg(unix)]
    let terminate = async {
        tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
            .expect("install SIGTERM handler")
            .recv()
            .await;
    };

    #[cfg(not(unix))]
    let terminate = std::future::pending::<()>();

    tokio::select! {
        _ = ctrl_c => {}
        _ = terminate => {}
    }

    tracing::info!("worker shutting down");
}

/// Verify the X-Worker-Signature header against body using secret.
/// Returns Ok(()) on success, Err with a static message on any failure.
/// The error detail is never forwarded to the caller — only UNAUTHORIZED is surfaced.
fn verify_worker_sig(header: &str, body: &[u8], secret: &[u8]) -> Result<(), &'static str> {
    let mut ts_str: Option<&str> = None;
    let mut sig_hex: Option<&str> = None;

    for part in header.split(',') {
        if let Some(v) = part.strip_prefix("t=") {
            ts_str = Some(v);
        } else if let Some(v) = part.strip_prefix("v1=") {
            sig_hex = Some(v);
        }
    }

    let ts_str = ts_str.ok_or("missing timestamp")?;
    let sig_hex = sig_hex.ok_or("missing signature")?;

    let ts: i64 = ts_str.parse().map_err(|_| "invalid timestamp")?;

    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_err(|_| "system time error")?
        .as_secs() as i64;

    if (now - ts).abs() > WORKER_SIG_WINDOW {
        return Err("stale timestamp");
    }

    let provided = hex::decode(sig_hex).map_err(|_| "invalid hex in signature")?;
    if provided.len() != 32 {
        return Err("invalid signature length");
    }

    let mut mac = HmacSha256::new_from_slice(secret).map_err(|_| "invalid key")?;
    mac.update(ts_str.as_bytes());
    mac.update(b".");
    mac.update(body);

    // verify_slice performs a constant-time comparison internally.
    mac.verify_slice(&provided).map_err(|_| "signature mismatch")
}

async fn health_handler() -> impl IntoResponse {
    (StatusCode::OK, Json(serde_json::json!({"status": "ok"})))
}

async fn invoke_handler(
    State(state): State<Arc<AppState>>,
    // Accept the full HTTP request so we can read headers before consuming the body.
    http_req: axum::extract::Request,
) -> AxumResponse {
    // Extract the signature header before consuming the body.
    let sig_header = http_req
        .headers()
        .get(WORKER_SIG_HEADER)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .to_owned();

    // Read body (10 MiB cap — matches the Go SDK limit).
    let body = match to_bytes(http_req.into_body(), 10 * 1024 * 1024).await {
        Ok(b) => b,
        Err(_) => {
            return error_envelope(WorkerError::new("BAD_REQUEST", "invalid request body"));
        }
    };

    // Verify HMAC-SHA256 channel-auth signature when configured.
    // None secret → skip verification → today's behaviour preserved (opt-in only).
    if let Some(secret) = &state.secret {
        if verify_worker_sig(&sig_header, &body, secret).is_err() {
            // Surface only a stable code; the signature detail is never echoed.
            return error_envelope(WorkerError::new("UNAUTHORIZED", "invalid request signature"));
        }
    }

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

    match state.handler.handle(req).await {
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
        // Use router_with_config with empty secret so existing tests are deterministic
        // regardless of the WORKER_SHARED_SECRET environment variable.
        router_with_config(
            |req: Request| async move {
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
            },
            HandlerConfig::default(), // empty secret = signing disabled
        )
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
        let app = router_with_config(
            |_req: Request| async move {
                Err(HandlerError::from(WorkerError::new(
                    "INVALID_VAT_FORMAT",
                    "VAT number format not recognised",
                )))
            },
            HandlerConfig::default(),
        );
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
        let app = router_with_config(
            |_req: Request| async move {
                let cause = std::io::Error::other("secret db dsn leaked here");
                Err::<Response, HandlerError>(HandlerError::internal(cause))
            },
            HandlerConfig::default(),
        );
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
        assert!(!json.to_string().contains("secret db dsn"));
    }

    #[tokio::test]
    async fn zero_billable_units_is_normalised_to_one() {
        let app = router_with_config(
            |_req: Request| async move {
                Ok(Response::new(serde_json::json!({})).with_units(0))
            },
            HandlerConfig::default(),
        );
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

    // --- verify_worker_sig unit tests ----------------------------------------
    // Test matrix mirrors billing/webhook_test.go: valid, missing, wrong-secret,
    // tampered-body, stale-timestamp, and disabled-path.

    fn make_sig(secret: &[u8], ts: i64, body: &[u8]) -> String {
        let ts_str = ts.to_string();
        let mut mac = HmacSha256::new_from_slice(secret).unwrap();
        mac.update(ts_str.as_bytes());
        mac.update(b".");
        mac.update(body);
        format!("t={},v1={}", ts_str, hex::encode(mac.finalize().into_bytes()))
    }

    fn now_ts() -> i64 {
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_secs() as i64
    }

    #[test]
    fn verify_valid_signature_accepted() {
        let secret = b"test-shared-secret";
        let body = b"test body";
        let header = make_sig(secret, now_ts(), body);
        assert!(verify_worker_sig(&header, body, secret).is_ok());
    }

    #[test]
    fn verify_missing_signature_rejected() {
        let secret = b"test-shared-secret";
        let body = b"test body";
        assert!(verify_worker_sig("", body, secret).is_err());
    }

    #[test]
    fn verify_wrong_secret_rejected() {
        let body = b"test body";
        let header = make_sig(b"correct-secret", now_ts(), body);
        assert!(verify_worker_sig(&header, body, b"wrong-secret").is_err());
    }

    #[test]
    fn verify_tampered_body_rejected() {
        let secret = b"test-shared-secret";
        let original = b"original body";
        let tampered = b"tampered body";
        let header = make_sig(secret, now_ts(), original);
        assert!(verify_worker_sig(&header, tampered, secret).is_err());
    }

    #[test]
    fn verify_stale_timestamp_rejected() {
        let secret = b"test-shared-secret";
        let body = b"test body";
        // 10 minutes ago = outside the 5-minute window
        let stale_ts = now_ts() - 600;
        let header = make_sig(secret, stale_ts, body);
        assert!(verify_worker_sig(&header, body, secret).is_err());
    }

    // --- HMAC-signed /invoke integration tests (using router_with_config) ----

    #[tokio::test]
    async fn signed_invoke_accepted_with_correct_secret() {
        const SECRET: &[u8] = b"integration-test-secret";
        let body = r#"{"operation":"test","payload":{}}"#;
        let header = make_sig(SECRET, now_ts(), body.as_bytes());

        let app = router_with_config(
            |_req: Request| async move { Ok(Response::new(serde_json::json!({"ok": true}))) },
            HandlerConfig { shared_secret: Some(SECRET.to_vec()) },
        );
        let resp = app
            .oneshot(
                HttpRequest::builder()
                    .method("POST")
                    .uri("/invoke")
                    .header(WORKER_SIG_HEADER, header)
                    .body(Body::from(body))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(resp.status(), StatusCode::OK);
        let json = body_json(resp).await;
        assert!(json.get("error").is_none(), "expected no error, got {json}");
    }

    #[tokio::test]
    async fn unsigned_invoke_rejected_when_secret_configured() {
        let app = router_with_config(
            |_req: Request| async move { Ok(Response::new(serde_json::json!({}))) },
            HandlerConfig { shared_secret: Some(b"secret".to_vec()) },
        );
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
        assert_eq!(json["error"]["code"], "UNAUTHORIZED");
    }

    #[tokio::test]
    async fn unsigned_invoke_succeeds_when_no_secret_configured() {
        let app = router_with_config(
            |_req: Request| async move { Ok(Response::new(serde_json::json!({}))) },
            HandlerConfig::default(), // no secret = signing disabled
        );
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
        assert!(json.get("error").is_none(), "expected success, got {json}");
    }
}
