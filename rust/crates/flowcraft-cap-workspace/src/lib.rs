//! The workspace capability.
//!
//! Three pieces, one contract:
//!
//! * [`local`] — the provider: a directory tree with path confinement.
//! * [`endpoint`] — a resource that serves a provider over gRPC, from a socket
//!   the host's [`flowcraft_kernel::EndpointHost`] created.
//! * [`remote`] — a resource that presents a remote endpoint as the same
//!   [`contract::Workspace`], so callers cannot tell local from remote.
//!
//! Everything in this crate is gated by a grant: the endpoint validates the
//! token offline, checks the endpoint/tenant/capability, and enforces the
//! scope's root and write flag per call.

#![cfg_attr(test, allow(clippy::unwrap_used, clippy::expect_used))]
pub mod contract;
pub mod endpoint;
pub mod local;
pub mod remote;
pub mod scope;

pub use contract::{Capabilities, Entry, Handle, KIND, Stat, Workspace};
pub use local::LocalWorkspace;
pub use remote::RemoteWorkspace;
