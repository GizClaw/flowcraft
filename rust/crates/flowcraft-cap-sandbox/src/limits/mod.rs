//! Resource caps.
//!
//! Two things are separated here on purpose:
//!
//! - **What this host can enforce** ([`Manager::support`]) is *probed*, not
//!   assumed: a real control group is created and removed once at construction,
//!   so a host where cgroups are not writable reports `false` instead of
//!   advertising a cap that would never fire.
//! - **The arithmetic** ([`cpu_budget_usec`]) is platform-independent and
//!   unit-tested everywhere: a millicores ceiling is a *budget* derived from the
//!   per-call timeout, because cgroups cap a *rate* and the two are not the same
//!   guarantee.
//!
//! A request for a cap this host cannot enforce is `NotAvailable`, never a
//! silent pass. When a cap was armed but can no longer be read (the control
//! group vanished), the session is killed with `Unenforceable` rather than run
//! unguarded — the same rule the Go side applies to its sampler.
//!
//! `disk_bytes` is rejected everywhere: no local backend has a quota mechanism.

#[cfg(target_os = "linux")]
mod cgroup;
#[cfg(target_os = "linux")]
use cgroup as backend;

#[cfg(not(target_os = "linux"))]
mod none;
#[cfg(not(target_os = "linux"))]
use none as backend;

use std::time::Duration;

use flowcraft_error::{Error, Result};

use crate::codes::{self, Code};
use crate::contract::{ExitReason, ResourceLimits};

/// What this host can enforce.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub(crate) struct Support {
    /// Memory caps are enforced.
    pub memory: bool,
    /// CPU budgets are enforced.
    pub cpu: bool,
}

/// The limits backend of one runner.
pub(crate) struct Manager {
    backend: backend::Backend,
}

impl std::fmt::Debug for Manager {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("sandbox.Limits")
            .field("support", &self.support())
            .finish_non_exhaustive()
    }
}

impl Manager {
    /// Probe this host once.
    pub(crate) fn probe() -> Self {
        Self {
            backend: backend::Backend::probe(),
        }
    }

    /// What this host enforces.
    pub(crate) fn support(&self) -> Support {
        self.backend.support()
    }

    /// Reject a request this host cannot honour.
    pub(crate) fn check(&self, limits: &ResourceLimits, timeout: Option<Duration>) -> Result<()> {
        if limits.disk_bytes.is_some() {
            return Err(codes::unavailable(
                Code::LimitsUnsupported,
                "sandbox: disk limits are not supported (no quota mechanism)",
            ));
        }
        let support = self.support();
        if limits.memory_bytes.is_some() && !support.memory {
            return Err(codes::unavailable(
                Code::LimitsUnsupported,
                "sandbox: memory limits need a writable cgroup v2 hierarchy, which is not available here",
            ));
        }
        if limits.cpu_millicores.is_some() {
            // The budget is derived from the timeout, so the two travel together.
            if timeout.is_none() {
                return Err(codes::unavailable(
                    Code::LimitsUnsupported,
                    "sandbox: cpu_millicores needs a per-call timeout to derive a cpu-time budget",
                ));
            }
            if !support.cpu {
                return Err(codes::unavailable(
                    Code::LimitsUnsupported,
                    "sandbox: cpu limits need a writable cgroup v2 hierarchy, which is not available here",
                ));
            }
        }
        Ok(())
    }

    /// Prepare the control group for a session that is about to start.
    ///
    /// Returns `None` when the request asks for no cap. Fails before anything is
    /// spawned, so a refused policy never leaves a process behind.
    pub(crate) fn plan(
        &self,
        id: &str,
        limits: &ResourceLimits,
        timeout: Option<Duration>,
    ) -> Result<Option<Plan>> {
        self.check(limits, timeout)?;
        if limits.memory_bytes.is_none() && limits.cpu_millicores.is_none() {
            return Ok(None);
        }
        let budget = limits
            .cpu_millicores
            .map(|millicores| cpu_budget_usec(millicores, timeout))
            .transpose()?;
        backend::plan(id, limits.memory_bytes, budget).map(|plan| Some(Plan(plan)))
    }
}

/// A prepared control group, waiting for the child's pid.
#[derive(Debug)]
pub(crate) struct Plan(backend::Plan);

impl Plan {
    /// Move the child into the group and start accounting.
    pub(crate) fn arm(self, pid: u32) -> Result<Armed> {
        self.0.arm(pid).map(Armed)
    }
}

/// A group that is accounting for a running child.
#[derive(Debug)]
pub(crate) struct Armed(backend::Armed);

impl Armed {
    /// Whether a cap was exceeded. `Err` means it can no longer be enforced.
    pub(crate) fn check(&mut self) -> Result<Option<ExitReason>> {
        self.0.check()
    }

    /// Release the group.
    pub(crate) fn close(self) {
        self.0.close();
    }
}

/// The cpu-time budget a millicores ceiling implies over `timeout`.
///
/// `CPUMillicores` is a share of one core, so a 500m ceiling over a 10s timeout
/// is 5 core-seconds — which is what a cgroup can actually account for.
pub(crate) fn cpu_budget_usec(millicores: u32, timeout: Option<Duration>) -> Result<u64> {
    let timeout = timeout
        .ok_or_else(|| Error::not_available("sandbox: cpu_millicores needs a per-call timeout"))?;
    let micros = u128::from(millicores) * timeout.as_micros() / 1000;
    u64::try_from(micros).map_err(|_| Error::validation("sandbox: cpu budget overflows"))
}

/// Parse one `key value` line out of a cgroup file.
///
/// `cpu.stat` and `memory.events` are both flat "name value" text, so one helper
/// covers both. It compiles on every platform so the parsing is testable
/// everywhere, even where no cgroup exists to read.
#[cfg_attr(not(target_os = "linux"), allow(dead_code))]
pub(crate) fn keyed_value(text: &str, key: &str) -> Option<u64> {
    text.lines().find_map(|line| {
        let mut parts = line.split_whitespace();
        let name = parts.next()?;
        if name != key {
            return None;
        }
        parts.next()?.parse().ok()
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_millicores_ceiling_becomes_a_budget_over_the_timeout() {
        // 500m for 10s is 5 core-seconds.
        assert_eq!(
            cpu_budget_usec(500, Some(Duration::from_secs(10))).unwrap(),
            5_000_000
        );
        // A full core for one second is one core-second.
        assert_eq!(
            cpu_budget_usec(1000, Some(Duration::from_secs(1))).unwrap(),
            1_000_000
        );
        // 100m for 500ms is 50ms of cpu.
        assert_eq!(
            cpu_budget_usec(100, Some(Duration::from_millis(500))).unwrap(),
            50_000
        );
    }

    #[test]
    fn a_budget_without_a_timeout_is_refused() {
        assert_eq!(
            cpu_budget_usec(500, None).unwrap_err().kind(),
            flowcraft_error::ErrorKind::NotAvailable
        );
    }

    #[test]
    fn a_cgroup_file_is_parsed_by_key() {
        let stat = "usage_usec 12345\nuser_usec 6000\nsystem_usec 6345\n";
        assert_eq!(keyed_value(stat, "usage_usec"), Some(12_345));
        assert_eq!(keyed_value(stat, "nonexistent"), None);

        let events = "low 0\nhigh 0\nmax 3\noom 1\noom_kill 1\n";
        assert_eq!(keyed_value(events, "oom_kill"), Some(1));
        assert_eq!(keyed_value(events, "low"), Some(0));
    }

    #[test]
    fn a_cap_this_host_cannot_enforce_is_refused_not_ignored() {
        let manager = Manager::probe();
        let support = manager.support();

        let mut limits = ResourceLimits {
            memory_bytes: Some(1024),
            ..ResourceLimits::default()
        };
        if support.memory {
            manager.check(&limits, None).expect("memory is supported");
        } else {
            assert_eq!(
                manager.check(&limits, None).unwrap_err().kind(),
                flowcraft_error::ErrorKind::NotAvailable
            );
        }

        limits = ResourceLimits {
            cpu_millicores: Some(100),
            ..ResourceLimits::default()
        };
        if support.cpu {
            // ... but it still needs a timeout to derive the budget.
            assert_eq!(
                manager.check(&limits, None).unwrap_err().kind(),
                flowcraft_error::ErrorKind::NotAvailable
            );
            manager
                .check(&limits, Some(Duration::from_secs(1)))
                .expect("cpu is supported");
        } else {
            assert_eq!(
                manager
                    .check(&limits, Some(Duration::from_secs(1)))
                    .unwrap_err()
                    .kind(),
                flowcraft_error::ErrorKind::NotAvailable
            );
        }
    }

    #[test]
    fn disk_limits_are_refused_everywhere() {
        let manager = Manager::probe();
        let limits = ResourceLimits {
            disk_bytes: Some(1),
            ..ResourceLimits::default()
        };
        assert_eq!(
            manager.check(&limits, None).unwrap_err().kind(),
            flowcraft_error::ErrorKind::NotAvailable
        );
    }

    #[test]
    fn a_request_without_caps_needs_no_plan() {
        let manager = Manager::probe();
        let plan = manager
            .plan("s-1", &ResourceLimits::default(), None)
            .expect("no caps");
        assert!(plan.is_none());
    }
}
