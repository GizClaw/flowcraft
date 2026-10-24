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
// A pty is a platform mechanism: the unix one is `portable-pty`, the Windows one
// is planned (W2) and is a stub that reports itself unavailable.
#[cfg(unix)]
pub(crate) mod terminal;
#[cfg(not(unix))]
#[path = "terminal_windows.rs"]
pub(crate) mod terminal;

pub(crate) use registry::{Registry, Spawn};
pub(crate) use spawn::{DEFAULT_OUTPUT_LIMIT, SpawnOptions, spawn};
