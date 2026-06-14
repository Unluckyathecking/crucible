//! Crucible Rust SDK
//!
//! This crate provides the SDK for building Crucible workers in Rust.
//! It mirrors the Go SDK at `workers/sdk-go/crucible.go` and communicates
//! with the Go gateway via HTTP/JSON using the tool.proto contract.

pub mod server;

pub use server::{
    router, router_with_config, serve, Handler, HandlerConfig, HandlerError, Request, Response,
    ServeError, WorkerError,
};
