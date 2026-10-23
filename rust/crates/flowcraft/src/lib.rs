//! Facade over the FlowCraft platform crates.
//!
//! One crate name for consumers, fine-grained crates underneath:
//!
//! ```text
//! flowcraft::error      errors and their categories
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
//! - `platform` (default) — documents, composition, control plane, client.
//! - `caps` — every built-in capability implementation.
//! - `error` — the error taxonomy alone.
//! - `telemetry` — the telemetry pipeline alone.
//! - `proto` — the generated wire types alone.
//!
//! # This crate is for consumers only
//!
//! Workspace crates depend on the specific crate they need, never on this
//! facade: a facade re-exporting a crate that depended on it would be a cycle.
//! Keeping the split also keeps a consumer from compiling the sandbox backend
//! because it wanted an error type.

#![cfg_attr(test, allow(clippy::unwrap_used, clippy::expect_used))]

#[cfg(feature = "error")]
pub use flowcraft_error as error;

#[cfg(feature = "telemetry")]
pub use flowcraft_telemetry as telemetry;

#[cfg(feature = "proto")]
pub use flowcraft_proto as proto;

#[cfg(feature = "platform")]
pub use flowcraft_resource as resource;

#[cfg(feature = "platform")]
pub use flowcraft_kernel as kernel;

#[cfg(feature = "platform")]
pub use flowcraft_client as client;

#[cfg(feature = "platform")]
pub use flowcraft_search as search;

/// The built-in capability implementations.
///
/// Each module is the whole capability: contract, local provider, endpoint
/// server, and remote client.
#[cfg(feature = "caps")]
pub mod caps {
    pub use flowcraft_cap_sandbox as sandbox;
    pub use flowcraft_cap_secret as secret;
    pub use flowcraft_cap_tool as tool;
    pub use flowcraft_cap_workspace as workspace;
}
