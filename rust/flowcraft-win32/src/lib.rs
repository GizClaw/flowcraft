//! The Win32 mechanisms the sandbox needs, behind a safe API.
//!
//! `flowcraft` forbids `unsafe` outright, and a `forbid` cannot be relaxed by an
//! inner `allow`. Job objects, security tokens, integrity labels and the WFP
//! filter engine are all raw Win32, so they live here: one crate that may use
//! `unsafe`, with a surface that is safe to call and an invariant documented at
//! every call site.
//!
//! The crate compiles everywhere and is empty off Windows, so a workspace build
//! does not need a Windows host.
//!
//! # What is implemented
//!
//! [`job`] — the process-tree boundary: a job object with `KILL_ON_JOB_CLOSE`,
//! kernel memory and cpu-time limits, and the accounting needed to say *why* a
//! session ended.
//!
//! [`process`] — the pipe session's spawn: three pipes, the child's ends
//! inherited under a handle list, plus the same token and job as a terminal.
//!
//! [`token`] — the write boundary: a Low-integrity token derived from ours, which
//! Windows enforces as "no write up" without any per-path ACL work.
//!
//! [`appcontainer`] — the stricter boundary's profile and SID: a container owns
//! nothing until an ACL grants it, which is what makes `read_only` and
//! `writable_paths` mean something. The SID is applied when a process is created
//! (a `PROC_THREAD_ATTRIBUTE_SECURITY_CAPABILITIES` entry), never by rewriting a
//! token — `SetTokenInformation(TokenAppContainerSid)` refuses, and a Windows lane
//! caught it.
//!
//! [`pty`] — the terminal: a ConPTY with the two pipes that carry its traffic,
//! the process creation that attaches a child to it with the flags and (from W3)
//! the token the sandbox chooses, and [`pty::CursorReport`], which answers the one
//! escape sequence a console asks its terminal for.

#![deny(missing_docs)]
#![cfg_attr(test, allow(clippy::unwrap_used, clippy::expect_used))]

#[cfg(windows)]
pub mod appcontainer;

#[cfg(windows)]
pub mod job;

#[cfg(windows)]
pub mod pty;

#[cfg(windows)]
pub mod process;

#[cfg(windows)]
pub mod token;

#[cfg(windows)]
pub use job::{Error, Job, Limits, Usage, Violation};

#[cfg(windows)]
pub use pty::{Child, Command, CursorReport, Launch, Pty};

#[cfg(windows)]
pub use process::Spawned;

#[cfg(windows)]
pub use appcontainer::AppContainer;

#[cfg(windows)]
pub use token::{Integrity, Token};
