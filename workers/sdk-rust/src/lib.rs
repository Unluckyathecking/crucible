//! Crucible Rust SDK
//!
//! SDK for building Crucible workers in Rust. Mirrors the Go SDK contract.

pub mod server;

pub use server::{serve, Handler, ServeError};

use serde::{Deserialize, Serialize};
use std::collections::HashMap;

/// Mirrors Go's `crucible.Request`
#[derive(Debug, Deserialize)]
pub struct Request {
    pub request_id: String,
    pub customer_id: String,
    pub operation: String,
    pub payload: serde_json::Value,
    pub plan: String,
    #[serde(default)]
    pub metadata: HashMap<String, String>,
}

/// Mirrors Go's `crucible.Response`
#[derive(Debug, Serialize)]
pub struct Response {
    pub payload: serde_json::Value,
    #[serde(default = "default_units")]
    pub billable_units: u64,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub units_label: Option<String>,
}

#[allow(dead_code)]
fn default_units() -> u64 {
    1
}

/// Mirrors Go's `crucible.Error` (renamed to avoid std::error::Error conflict)
#[derive(Debug, Serialize)]
pub struct WorkerError {
    pub code: String,
    pub message: String,
    pub retryable: bool,
}

impl std::fmt::Display for WorkerError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}: {}", self.code, self.message)
    }
}

impl std::error::Error for WorkerError {}