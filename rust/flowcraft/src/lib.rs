//! The FlowCraft platform: wire contracts, deployment composition, the control
//! plane, and the built-in capabilities.
//!
//! ```text
//! flowcraft::error      errors and their categories        (always compiled)
//! flowcraft::telemetry  telemetry pipeline: vocabulary, rendering, sinks
//! flowcraft::proto      generated wire types
//! flowcraft::resource   deployment document + composition kernel
//! flowcraft::kernel     control plane: config, catalog, grants, listeners
//! flowcraft::client     grant-carrying client wire + craft ABI client
//! flowcraft::search     text retrieval primitives (tokenize + BM25)
//! flowcraft::caps::*    workspace / secret / sandbox / tool
//! ```
//!
//! # Features
//!
//! Every module below is behind the feature that names it, and the features
//! imply each other along the same one-way edges the modules have: a capability
//! needs `resource`, `resource` needs `telemetry`, and `telemetry` needs
//! nothing but the error taxonomy.
//!
//! - `platform` (default) — documents, composition, control plane, client.
//! - `caps` — every built-in capability, including the endpoint servers and
//!   remote clients that need the control plane (`wire`).
//! - `workspace` / `secret` / `sandbox` / `tool` — one capability's provider.
//!   None of them needs the control plane on its own: `wire` adds the endpoint
//!   server, the remote client and the grant-scope checks on top. Every one of
//!   these shapes compiles for a Windows target — the kernel serves `dns://`
//!   targets over TCP there and refuses a unix target at bind time.
//! - `telemetry`, `proto`, `search`, `resource`, `kernel`, `client`, `wire` —
//!   the layers underneath, for a consumer that wants less than `platform`.
//! - `otel` / `otlp` — trace context, and shipping it to a collector.
//!
//! ## Building without `protoc`
//!
//! The wire bindings under `proto` are committed, so nothing here runs a
//! build script or needs `protoc` on the machine that compiles this crate.
//! `make rust-proto-gen` regenerates them from `proto/` at the repository root,
//! which stays the single source of truth shared with the Go SDK.

// Tests are exempt from the async/blocking gate (see `clippy.toml`): a test may
// read a fixture with `std::fs` directly instead of standing up a runtime.
#![cfg_attr(
    test,
    allow(clippy::unwrap_used, clippy::expect_used, clippy::disallowed_methods)
)]

pub mod error;

#[cfg(feature = "blocking")]
pub mod blocking;

pub mod fs;

pub mod net;

#[cfg(feature = "telemetry")]
pub mod telemetry;

#[cfg(feature = "proto")]
pub mod proto;

#[cfg(feature = "message")]
pub mod message;

#[cfg(feature = "craft")]
pub mod craft;

#[cfg(feature = "search")]
pub mod search;

#[cfg(feature = "resource")]
pub mod resource;

#[cfg(feature = "kernel")]
pub mod kernel;

#[cfg(feature = "client")]
pub mod client;

pub mod caps;
