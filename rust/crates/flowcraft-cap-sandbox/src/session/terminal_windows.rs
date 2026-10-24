//! The Windows pty, not yet implemented.
//!
//! This is not a gap that hides: [`available`] is `false`, so the runner reports
//! `tty: false` and a `tty: true` request is refused with `NotAvailable` instead
//! of starting a session whose output nobody can read.
//!
//! The implementation is planned in `docs/plans/2026-10-23-rust-sandbox-windows.md`
//! (W2): ConPTY created by `flowcraft-win32` rather than by `portable-pty`,
//! because the Windows confinement phases need `CreateProcessAsUser` with *our*
//! token and `CREATE_SUSPENDED`/`CREATE_NEW_PROCESS_GROUP` — flags and a token that
//! a library which owns process creation cannot be given.

use std::sync::Arc;

use flowcraft_error::{Error, Result};

use crate::codes::{self, Code};
use crate::contract::Session;
use crate::contract::SessionSpec;
use crate::session::spawn::SpawnOptions;

/// Whether this build can provide a pty.
pub(crate) const fn available() -> bool {
    false
}

/// Refuse, honestly, until the ConPTY backend lands.
pub(crate) async fn spawn(_spec: SessionSpec, _options: &SpawnOptions) -> Result<Arc<dyn Session>> {
    Err(codes::unavailable(
        Code::TtyUnsupported,
        "sandbox: no pty backend on this platform yet; start the session with tty: false",
    ))
}
