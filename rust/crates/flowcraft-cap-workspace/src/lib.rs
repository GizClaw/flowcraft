//! The workspace capability.
//!
//! One contract, six pieces:
//!
//! * [`contract`] — the [`Workspace`] trait, the capability bits, and the
//!   [`Handle`] a factory produces.
//! * [`local`] — the provider: a directory tree pinned by a capability handle,
//!   so a symlink cannot walk out of the root.
//! * [`endpoint`] — a resource that serves a provider over gRPC, from a socket
//!   the host's [`flowcraft_kernel::EndpointHost`] created.
//! * [`remote`] — a resource that presents a remote endpoint as the same
//!   [`contract::Workspace`], so callers cannot tell local from remote.
//! * [`scoped`] — a decorator that refuses paths a glob policy does not allow,
//!   and records what it refused.
//! * [`sub`] — a decorator that presents one subtree as a whole workspace.
//!
//! [`helpers`] adds the composed operations every provider inherits (atomic
//! publish, copy, bounded reads, `walk`, `glob`); [`pattern`] is the single glob
//! spelling those helpers and the policy share.
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
pub mod endpoint;
pub mod helpers;
pub mod local;
pub mod path;
pub mod pattern;
pub mod remote;
pub mod scope;
pub mod scoped;
pub mod sub;

pub use contract::{Capabilities, Entry, Handle, KIND, Stat, Workspace};
pub use helpers::{Step, atomic_write, capped, copy, glob, walk};
pub use local::LocalWorkspace;
pub use remote::RemoteWorkspace;
pub use scoped::{LogSink, Operation, Policy, ScopedWorkspace, ViolationRecord, ViolationSink};
pub use sub::SubWorkspace;
