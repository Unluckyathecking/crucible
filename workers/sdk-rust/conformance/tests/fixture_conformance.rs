//! Fixture-driven conformance tests for the Crucible Rust SDK.
//!
//! Loads workers/conformance/fixture.json (the language-neutral spec) and asserts
//! each case against an in-process axum router built from the Rust SDK.
//! This is the single source of truth for Rust contract parity: adding a new
//! contract behaviour means adding it to the JSON fixture and implementing the
//! corresponding `run_rust_fixture_case` arm.

use axum::body::{to_bytes, Body};
use axum::http::{Request, StatusCode};
use crucible_sdk::{router_with_config, HandlerConfig, HandlerError, Request as WorkerReq, Response, WorkerError};
use serde::Deserialize;
use std::collections::HashMap;
use tower::ServiceExt;

// ── Fixture types ────────────────────────────────────────────────────────────

#[derive(Debug, Deserialize)]
#[allow(dead_code)]
struct Divergence {
    status: u16,
    note: String,
}

#[derive(Debug, Deserialize)]
struct Case {
    id: String,
    description: String,
    kind: String,
    #[serde(default)]
    known_divergences: HashMap<String, Divergence>,
}

#[derive(Debug, Deserialize)]
#[allow(dead_code)]
struct Fixture {
    version: String,
    cases: Vec<Case>,
}

fn load_fixture() -> Fixture {
    let path = format!("{}/../../conformance/fixture.json", env!("CARGO_MANIFEST_DIR"));
    let data = std::fs::read_to_string(&path)
        .unwrap_or_else(|e| panic!("load fixture {path}: {e}"));
    serde_json::from_str(&data).unwrap_or_else(|e| panic!("parse fixture {path}: {e}"))
}

// ── Shared builder helpers ────────────────────────────────────────────────────

fn echo_router() -> axum::Router {
    router_with_config(
        |_req: WorkerReq| async move { Ok(Response::new(serde_json::json!({"ok": true}))) },
        HandlerConfig::default(),
    )
}

async fn body_json(resp: axum::response::Response) -> serde_json::Value {
    let bytes = to_bytes(resp.into_body(), usize::MAX).await.unwrap();
    serde_json::from_slice(&bytes).unwrap()
}

// ── Per-case assertion helpers ────────────────────────────────────────────────

async fn assert_healthz_body(router: axum::Router) {
    let resp = router
        .oneshot(
            Request::builder()
                .method("GET")
                .uri("/healthz")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::OK, "healthz must return 200");

    let ct = resp
        .headers()
        .get("content-type")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("");
    assert!(
        ct.starts_with("application/json"),
        "healthz Content-Type must be application/json, got {ct:?}"
    );

    let body_bytes = to_bytes(resp.into_body(), usize::MAX).await.unwrap();
    let body = std::str::from_utf8(&body_bytes).unwrap();
    // axum's Json responder produces {"status":"ok"}\n — strip a trailing newline.
    let body = body.trim_end_matches('\n');
    assert_eq!(
        body, r#"{"status":"ok"}"#,
        "healthz body must be exactly {{\"status\":\"ok\"}}"
    );
}

async fn assert_method_not_allowed(router: axum::Router, expected_status: u16) {
    // axum returns 405 automatically for registered routes that receive an unsupported method.
    for method in &["GET", "HEAD", "PUT", "DELETE", "PATCH", "OPTIONS"] {
        let resp = router
            .clone()
            .oneshot(
                Request::builder()
                    .method(*method)
                    .uri("/invoke")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(
            resp.status().as_u16(),
            expected_status,
            "{method} /invoke must return {expected_status}"
        );
    }
}

async fn assert_billable_units_floor() {
    let router = router_with_config(
        |_req: WorkerReq| async move {
            // Explicitly return 0 units to exercise the SDK normalisation guard.
            Ok(Response::new(serde_json::json!({"floor": "ok"})).with_units(0))
        },
        HandlerConfig::default(),
    );
    let resp = router
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/invoke")
                .header("content-type", "application/json")
                .body(Body::from(r#"{"operation":"floor_test","payload":{}}"#))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    let json = body_json(resp).await;
    let units = json["billable_units"].as_u64().unwrap_or(0);
    assert!(
        units >= 1,
        "billable_units must be >= 1 after SDK normalisation, got {units}"
    );
}

async fn assert_apierror_envelope() {
    const ERROR_CODE: &str = "FIXTURE_TEST_ERROR";
    let router = router_with_config(
        |_req: WorkerReq| async move {
            Err::<Response, HandlerError>(HandlerError::from(
                WorkerError::new(ERROR_CODE, "fixture-driven error test").retryable(true),
            ))
        },
        HandlerConfig::default(),
    );
    let resp = router
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/invoke")
                .header("content-type", "application/json")
                .body(Body::from(r#"{"operation":"err_test","payload":{}}"#))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(
        resp.status(),
        StatusCode::OK,
        "error envelope must return HTTP 200"
    );
    let json = body_json(resp).await;
    let err = json.get("error").expect("error field must be present in envelope");
    assert_eq!(
        err["code"].as_str().unwrap_or(""),
        ERROR_CODE,
        "error.code must match"
    );
    assert!(
        !err["message"].as_str().unwrap_or("").is_empty(),
        "error.message must be non-empty"
    );
    assert!(
        err.get("retryable").is_some(),
        "error.retryable must be present"
    );
    assert!(
        json.get("payload").is_none(),
        "error envelope must not contain payload"
    );
    assert!(
        json.get("billable_units").is_none(),
        "error envelope must not contain billable_units"
    );
}

async fn assert_empty_body_bad_request(router: axum::Router) {
    // Empty body is not valid JSON; the SDK must return BAD_REQUEST.
    let resp = router
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/invoke")
                .header("content-type", "application/json")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(
        resp.status(),
        StatusCode::OK,
        "error envelopes always return HTTP 200"
    );
    let json = body_json(resp).await;
    let code = json["error"]["code"].as_str().unwrap_or("");
    assert_eq!(code, "BAD_REQUEST", "empty body must yield BAD_REQUEST, got {code:?}");
}

// ── Test entry point ──────────────────────────────────────────────────────────

#[tokio::test]
async fn fixture_driven_conformance() {
    let fixture = load_fixture();
    assert!(
        !fixture.cases.is_empty(),
        "shared fixture loaded zero cases; check workers/conformance/fixture.json"
    );

    let echo = echo_router();

    for case in &fixture.cases {
        if let Some(div) = case.known_divergences.get("rust") {
            eprintln!("SKIP {}: known Rust divergence: {}", case.id, div.note);
            continue;
        }

        eprintln!("RUN  {} — {}", case.id, case.description);

        // For cases that need the expected status from the fixture (method_not_allowed),
        // fall back to the contract default (405) when no known divergence is present.
        let expected_status: u16 = 405;

        match case.kind.as_str() {
            "healthz_body" => assert_healthz_body(echo.clone()).await,
            "method_not_allowed" => assert_method_not_allowed(echo.clone(), expected_status).await,
            "billable_units_floor" => assert_billable_units_floor().await,
            "apierror_envelope" => assert_apierror_envelope().await,
            "empty_body_bad_request" => assert_empty_body_bad_request(echo.clone()).await,
            other => panic!("unknown fixture case kind {other:?} (id={}): update fixture_driven_conformance", case.id),
        }

        eprintln!("PASS {}", case.id);
    }
}
