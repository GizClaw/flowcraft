//! The workspace capability.
//!
//! One contract, six pieces:
//!
//! * [`contract`] — the [`Workspace`] trait, the capability bits, and the
//!   [`Handle`] a factory produces.
//! * [`local`] — the provider: a directory tree pinned by a capability handle,
//!   so a symlink cannot walk out of the root.
//! * [`scoped`] — a decorator that refuses paths a glob policy does not allow,
//!   and records what it refused.
//! * [`sub`] — a decorator that presents one subtree as a whole workspace.
//! * `endpoint` — a resource that serves a provider over gRPC, from a socket
//!   the host's `EndpointHost` created.
//! * `remote` — a resource that presents a remote endpoint as the same
//!   [`contract::Workspace`], so callers cannot tell local from remote.
//!
//! (`endpoint` and `remote` are spelled as code rather than links: they only
//! exist with the `wire` feature, and a link that resolves in one build and not
//! the other is a doc warning waiting to happen.)
//!
//! [`helpers`] adds the composed operations every provider inherits (atomic
//! publish, copy, bounded reads, `walk`, `glob`). The `pattern` and `path`
//! modules are the single spellings of "which paths a rule names" and "what a
//! path means" that the helpers, the decorators and the provider share, and
//! `retry` holds the bounded retry Windows needs.
//!
//! `endpoint`, `remote` and `scope` are behind the default `wire` feature.
//! Turning it off leaves a crate that only needs a filesystem — no control
//! plane, no transport — which is what a consumer that just wants a confined
//! directory should compile, and what the Windows check lane builds.
//!
//! # Two boundaries, one contract
//!
//! The wire-facing boundary is the grant: the endpoint validates the token
//! offline, checks the endpoint/tenant/capability, and enforces the scope's root
//! and write flag on every call. The in-process boundary is [`scoped`]: glob
//! rules that hold no matter how the provider was reached. Neither replaces the
//! other — a grant says what a remote caller may ask for, a policy says what
//! this deployment will hand out.
//!
//! Path confinement is the provider's job ([`local`]); the decorators map and
//! filter names, they do not resolve them.

#![cfg_attr(test, allow(clippy::unwrap_used, clippy::expect_used))]
pub mod contract;
pub mod helpers;
pub mod local;
mod path;
mod pattern;
mod retry;
pub mod scoped;
pub mod sub;

#[cfg(feature = "wire")]
pub mod endpoint;
#[cfg(feature = "wire")]
pub mod remote;
#[cfg(feature = "wire")]
pub mod scope;

pub use contract::{Capabilities, Entry, Handle, KIND, Stat, Workspace};
pub use helpers::{Step, atomic_write, capped, copy, glob, walk};
pub use local::LocalWorkspace;
#[cfg(feature = "wire")]
pub use remote::RemoteWorkspace;
pub use scoped::{LogSink, Operation, Policy, ScopedWorkspace, ViolationRecord, ViolationSink};
pub use sub::SubWorkspace;
