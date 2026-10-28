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
// The Windows process tree: job objects are the only way to own a whole tree
// there, and they carry the caps. Unix uses process groups inside `spawn`.
#[cfg(windows)]
pub(crate) mod job;

// A pty is a platform mechanism: `portable-pty` on unix, `flowcraft_win32::Pty`
// (`CreatePseudoConsole`) on Windows. Two implementations of one interface, chosen
// by `terminal::available()` so `Capabilities` cannot advertise a terminal the
// host does not have.
#[cfg(unix)]
pub(crate) mod terminal;
#[cfg(not(unix))]
#[path = "terminal_windows.rs"]
pub(crate) mod terminal;

pub(crate) use registry::{
    DEFAULT_EXIT_RETENTION_SECS, DEFAULT_MAX_SESSIONS, DEFAULT_MAX_TOTAL_LOG_BYTES, Registry,
    SessionLimits, Spawn,
};
pub(crate) use spawn::{DEFAULT_OUTPUT_LIMIT, SpawnOptions, spawn};
