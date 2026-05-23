use crate::{Request, Response, WorkerError};
use axum::{
    extract::DefaultBodyLimit,
    http::StatusCode,
    response::IntoResponse,
    routing::{get, post},
    Json, Router,
};
use std::sync::Arc;

pub type ServeError = Box<dyn std::error::Error + Send + Sync>;

#[async_trait::async_trait]
pub trait Handler: Send + Sync + 'static {
    async fn handle(&self, req: Request) -> Result<Response, WorkerError>;
}

pub async fn serve(port: u16, handler: impl Handler) -> Result<(), ServeError> {
    let h = Arc::new(handler);
    let app = Router::new()
        .route("/healthz", get(health_handler))
        .route("/readyz", get(health_handler))
        .route(
            "/invoke",
            post({
                let h = h.clone();
                move |body: Json<Request>| invoke_handler(h.clone(), body)
            }),
        )
        .layer(DefaultBodyLimit::max(10 * 1024 * 1024));

    let listener = tokio::net::TcpListener::bind(format!("0.0.0.0:{port}")).await?;
    tracing::info!(port, "worker listening");

    axum::serve(listener, app)
        .with_graceful_shutdown(shutdown_signal())
        .await?;
    Ok(())
}

async fn health_handler() -> impl IntoResponse {
    Json(serde_json::json!({"status": "ok"}))
}

async fn invoke_handler(
    handler: Arc<impl Handler>,
    Json(req): Json<Request>,
) -> impl IntoResponse {
    match handler.handle(req).await {
        Ok(mut resp) => {
            if resp.billable_units == 0 {
                resp.billable_units = 1;
            }
            (StatusCode::OK, Json(serde_json::to_value(resp).unwrap_or(serde_json::json!({}))))
        }
        Err(err) => {
            tracing::info!(code = %err.code, "handler returned structured error");
            let body = serde_json::json!({"error": err});
            (StatusCode::OK, Json(body))
        }
    }
}

async fn shutdown_signal() {
    let ctrl_c = async {
        tokio::signal::ctrl_c()
            .await
            .expect("failed to install Ctrl+C handler");
    };

    #[cfg(unix)]
    let terminate = async {
        tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
            .expect("failed to install SIGTERM handler")
            .recv()
            .await;
    };

    #[cfg(not(unix))]
    let terminate = std::future::pending::<()>();

    tokio::select! {
        _ = ctrl_c => {},
        _ = terminate => {},
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::body::Body;
    use axum::http::{Request as HttpRequest, StatusCode};
    use tower::ServiceExt;

    struct TestHandler;

    #[async_trait::async_trait]
    impl Handler for TestHandler {
        async fn handle(&self, _req: Request) -> Result<Response, WorkerError> {
            Ok(Response {
                payload: serde_json::json!({"ok": true}),
                billable_units: 1,
                units_label: None,
            })
        }
    }

    struct ErrorHandler;

    #[async_trait::async_trait]
    impl Handler for ErrorHandler {
        async fn handle(&self, _req: Request) -> Result<Response, WorkerError> {
            Err(WorkerError {
                code: "BAD_REQUEST".to_string(),
                message: "invalid input".to_string(),
                retryable: false,
            })
        }
    }

    #[tokio::test]
    async fn healthz_returns_ok() {
        let app = Router::new().route("/healthz", get(health_handler));
        let response = app
            .oneshot(HttpRequest::builder().uri("/healthz").body(Body::empty()).unwrap())
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::OK);
    }

    #[tokio::test]
    async fn readyz_returns_ok() {
        let app = Router::new().route("/readyz", get(health_handler));
        let response = app
            .oneshot(HttpRequest::builder().uri("/readyz").body(Body::empty()).unwrap())
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::OK);
    }

    #[tokio::test]
    async fn invoke_returns_success() {
        let h = Arc::new(TestHandler);
        let app = Router::new().route(
            "/invoke",
            post(move |body: Json<Request>| invoke_handler(h.clone(), body)),
        );
        let req = HttpRequest::builder()
            .uri("/invoke")
            .method("POST")
            .header("Content-Type", "application/json")
            .body(Body::from(r#"{"request_id":"r1","customer_id":"c1","operation":"test","payload":null,"plan":"pro","metadata":{}}"#))
            .unwrap();
        let response = app.oneshot(req).await.unwrap();
        assert_eq!(response.status(), StatusCode::OK);
    }

    #[tokio::test]
    async fn invoke_returns_error_envelope() {
        let h = Arc::new(ErrorHandler);
        let app = Router::new().route(
            "/invoke",
            post(move |body: Json<Request>| invoke_handler(h.clone(), body)),
        );
        let req = HttpRequest::builder()
            .uri("/invoke")
            .method("POST")
            .header("Content-Type", "application/json")
            .body(Body::from(r#"{"request_id":"r1","customer_id":"c1","operation":"test","payload":null,"plan":"pro","metadata":{}}"#))
            .unwrap();
        let response = app.oneshot(req).await.unwrap();
        assert_eq!(response.status(), StatusCode::OK);
    }
}