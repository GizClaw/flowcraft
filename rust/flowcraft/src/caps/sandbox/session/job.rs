//! The Windows process tree of one session.
//!
//! Windows has no process groups; a job object is the only primitive that owns a
//! whole tree. It is also where the caps live (`JobMemoryLimit` is a kernel
//! ceiling, `PerJobUserTimeLimit` is the `cpu_millicores × timeout` budget) and
//! what makes "close the session" mean "nothing it started is still running":
//! the job carries `KILL_ON_JOB_CLOSE`.
//!
//! Membership is inherited, and the job is attached at creation time
//! (`PROC_THREAD_ATTRIBUTE_JOB_LIST` in [`flowcraft_win32::process::spawn`] and
//! [`flowcraft_win32::Pty::spawn`]), so there is no window in which the tree is
//! outside the caps or outside `terminate`.
//!
//! What is decided *here* is policy: the cpu budget a `cpu_millicores` ceiling
//! implies over a timeout, and the contract's vocabulary for a spent cap. How to
//! ask whether a cap is spent belongs to the job itself
//! ([`flowcraft_win32::Job::exceeded`]) — that answer is built on Win32
//! accounting semantics, which is exactly the kind of knowledge this file could
//! not own and could not test.

use std::time::Duration;

use crate::caps::sandbox::contract::{ExitReason, ResourceLimits};
use crate::error::{Error, Result};

/// The job object of one session.
#[derive(Clone)]
pub(crate) struct SessionJob {
    job: flowcraft_win32::Job,
}

impl std::fmt::Debug for SessionJob {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("sandbox.SessionJob")
    }
}

impl SessionJob {
    /// Probe: can this host create a job at all?
    pub(crate) fn probe() -> bool {
        flowcraft_win32::Job::create(flowcraft_win32::Limits::default()).is_ok()
    }

    /// Create the tree boundary, with the caps the spec asked for.
    pub(crate) fn create(limits: &ResourceLimits, timeout: Option<Duration>) -> Result<Self> {
        let wanted = flowcraft_win32::Limits {
            memory_bytes: limits.memory_bytes,
            cpu_budget: budget(limits, timeout)?,
        };
        let job = flowcraft_win32::Job::create(wanted).map_err(map)?;
        Ok(Self { job })
    }

    /// The job itself, for the one caller that attaches a child at creation
    /// time (`flowcraft_win32::process::spawn` / `Pty::spawn`).
    pub(crate) fn job(&self) -> &flowcraft_win32::Job {
        &self.job
    }

    /// Kill the whole tree.
    pub(crate) fn terminate(&self) -> Result<()> {
        self.job.terminate(1).map_err(map)
    }

    /// Whether the tree has spent its budget.
    ///
    /// The question — "did this hit the cap or did it exit?" — is the job's to
    /// answer, because the answer is built on Win32 accounting semantics: the
    /// kernel's violation record answers zeroed on some hosts, `JobMemoryLimit`
    /// makes allocations fail rather than killing the tree, and the fields a
    /// caller asked for are not necessarily the ones in force. All of that lives
    /// in [`flowcraft_win32::Job::exceeded`]; what is left here is the sandbox's
    /// vocabulary for it.
    pub(crate) fn violation(&self) -> Option<ExitReason> {
        match self.job.exceeded() {
            Ok(Some(_)) => Some(ExitReason::BudgetExceeded),
            Ok(None) => None,
            Err(error) => {
                tracing::debug!(%error, "sandbox: the job's accounting is unavailable");
                None
            }
        }
    }
}

/// The cpu-time budget a `cpu_millicores` ceiling implies over `timeout`.
///
/// The same arithmetic the unix backend uses, so the two platforms classify a
/// budget identically: a share of one core for the session's lifetime.
fn budget(limits: &ResourceLimits, timeout: Option<Duration>) -> Result<Option<Duration>> {
    let Some(millicores) = limits.cpu_millicores else {
        return Ok(None);
    };
    let Some(timeout) = timeout else {
        return Err(Error::not_available(
            "sandbox: cpu_millicores needs a per-call timeout to derive a cpu-time budget",
        ));
    };
    let micros = u128::from(millicores) * timeout.as_micros() / 1000;
    Ok(Some(Duration::from_micros(
        u64::try_from(micros).unwrap_or(u64::MAX),
    )))
}

fn map(error: flowcraft_win32::Error) -> Error {
    Error::not_available(format!("sandbox: {error}"))
}
