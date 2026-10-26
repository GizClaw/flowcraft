//! The Windows process tree of one session.
//!
//! Windows has no process groups; a job object is the only primitive that owns a
//! whole tree. It is also where the caps live (`JobMemoryLimit` is a kernel
//! ceiling, `PerJobUserTimeLimit` is the `cpu_millicores × timeout` budget) and
//! what makes "close the session" mean "nothing it started is still running":
//! the job carries `KILL_ON_JOB_CLOSE`.
//!
//! Membership is inherited, so assigning the leader right after the spawn covers
//! everything it forks. The one honest gap: a grandchild forked between `spawn`
//! and the assignment is outside the job until it forks again. `CreateProcess`
//! with `CREATE_SUSPENDED` would close that window and needs a creation flag the
//! tokio spawn path does not expose; it is tracked as W1's follow-up rather than
//! papered over.

use std::time::Duration;

use crate::caps::sandbox::contract::{ExitReason, ResourceLimits};
use crate::error::{Error, Result};

/// The job object of one session.
#[derive(Clone)]
pub(crate) struct SessionJob {
    job: flowcraft_win32::Job,
    /// What was asked for, so the counters can be compared against it.
    limits: flowcraft_win32::Limits,
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
        Ok(Self {
            job,
            limits: wanted,
        })
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
    /// Two sources, in order of authority. The kernel's violation record is the
    /// direct answer, but on this platform the legacy
    /// `JobObjectLimitViolationInformation` query answers zeroed, so the counters
    /// are what the answer is really built on: a tree whose cpu time has reached
    /// the budget, or whose peak commit has reached the ceiling, is over it.
    ///
    /// A memory ceiling is subtler than cpu time: `JobMemoryLimit` makes
    /// allocations fail past the ceiling rather than killing the tree, so a
    /// program that never gives up has to be ended here, which is what makes the
    /// contract's "over the cap ends the session" true on Windows too.
    pub(crate) fn violation(&self) -> Option<ExitReason> {
        match self.job.violation() {
            Ok(Some(_)) => return Some(ExitReason::BudgetExceeded),
            Ok(None) => {}
            Err(error) => {
                tracing::debug!(%error, "sandbox: the job's violation state is unavailable");
            }
        }
        let usage = match self.job.usage() {
            Ok(usage) => usage,
            Err(error) => {
                tracing::debug!(%error, "sandbox: the job's accounting is unavailable");
                return None;
            }
        };
        let cpu_spent = self
            .limits
            .cpu_budget
            .is_some_and(|budget| usage.user_time >= budget);
        let memory_spent = self
            .limits
            .memory_bytes
            .is_some_and(|cap| usage.peak_memory >= cap);
        (cpu_spent || memory_spent).then_some(ExitReason::BudgetExceeded)
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
