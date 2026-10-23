//! Session machinery shared by every local backend.
//!
//! Three pieces, each with one job:
//!
//! - [`log`] is the replayable, byte-cursor output buffer and the event fan-out;
//! - [`registry`] is the one place sessions are remembered;
//! - [`spawn`] is the only place a child process is created.
//!
//! The confined backends rewrite a spec and delegate to [`spawn`], so a
//! `bwrap` session and a `local` session cannot drift in cursor, termination, or
//! exit-classification semantics.

pub(crate) mod log;
pub(crate) mod registry;
pub(crate) mod spawn;

pub(crate) use registry::{Registry, Spawn};
pub(crate) use spawn::{DEFAULT_OUTPUT_LIMIT, spawn};
