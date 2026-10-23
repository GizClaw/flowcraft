//! The cgroup v2 backend.
//!
//! One control group per session, under `flowcraft/<session-id>`. The group is
//! what makes the two caps enforceable:
//!
//! - **memory**: `memory.max` is a hard ceiling the kernel applies, and
//!   `memory.events` reports `oom_kill` once it fired — no sampling, no estimate;
//! - **cpu**: cgroups cap a *rate*, while the contract is a *budget*
//!   (`timeout × millicores`), so the group's cumulative `cpu.stat` is compared
//!   with the budget and the group is killed when it is spent.
//!
//! Membership is inherited: attaching the leader is enough for every child it
//! forks, which is why one `add_task` covers the whole tree.
//!
//! cgroup **v1** is reported as unsupported rather than half-implemented: its
//! files, controller names and accounting differ, and a cap that silently does
//! not fire is worse than one that is refused.

use std::path::PathBuf;

use cgroups_rs::CgroupPid;
use cgroups_rs::fs::Cgroup;
use cgroups_rs::fs::Controller;
use cgroups_rs::fs::cgroup_builder::CgroupBuilder;
use cgroups_rs::fs::cpu::CpuController;
use cgroups_rs::fs::memory::MemController;
use flowcraft_error::{Error, Result};

use super::{Support, keyed_value};
use crate::contract::ExitReason;

/// Where the probe and the sessions live under the hierarchy root.
const PREFIX: &str = "flowcraft";

/// The backend of this host.
#[derive(Debug)]
pub(crate) struct Backend {
    support: Support,
}

impl Backend {
    /// Probe once: try to create, limit and remove a real control group.
    pub(crate) fn probe() -> Self {
        Self {
            support: probe().unwrap_or_default(),
        }
    }

    pub(crate) fn support(&self) -> Support {
        self.support
    }
}

/// A real attempt, not a guess: create a group, write a limit, remove it.
fn probe() -> Option<Support> {
    let cgroup = CgroupBuilder::new(&format!("{PREFIX}-probe"))
        .memory()
        .memory_hard_limit(i64::MAX)
        .done()
        .cpu()
        .shares(1024)
        .done()
        .build(cgroups_rs::fs::hierarchies::auto())
        .ok()?;
    // v1 differs enough that claiming the same semantics would be a lie.
    let unified = cgroup.v2();
    // Reading the accounting files is what a session actually needs.
    let readers_work = read(&cgroup, "memory.events").is_ok() && read(&cgroup, "cpu.stat").is_ok();
    let _ = cgroup.delete();
    if !unified || !readers_work {
        return None;
    }
    Some(Support {
        memory: true,
        cpu: true,
    })
}

/// A prepared control group.
#[derive(Debug)]
pub(crate) struct Plan {
    cgroup: Cgroup,
    memory: bool,
    cpu_budget_usec: Option<u64>,
}

pub(crate) fn plan(id: &str, memory: Option<u64>, cpu_budget_usec: Option<u64>) -> Result<Plan> {
    let name = format!("{PREFIX}/{id}");
    let mut builder = CgroupBuilder::new(&name);
    if let Some(bytes) = memory {
        let limit = i64::try_from(bytes).unwrap_or(i64::MAX);
        builder = builder.memory().memory_hard_limit(limit).done();
    }
    if cpu_budget_usec.is_some() {
        // The weight is untouched (1024 = the default); the group is created so
        // that `cpu.stat` accounts for this process tree.
        builder = builder.cpu().shares(1024).done();
    }
    let cgroup = builder
        .build(cgroups_rs::fs::hierarchies::auto())
        .map_err(|error| {
            Error::not_available(format!("sandbox: create control group {name:?}: {error}"))
        })?;
    Ok(Plan {
        cgroup,
        memory: memory.is_some(),
        cpu_budget_usec,
    })
}

impl Plan {
    /// Attach the child and start accounting.
    pub(crate) fn arm(self, pid: u32) -> Result<Armed> {
        self.cgroup
            .add_task(CgroupPid::from(u64::from(pid)))
            .map_err(|error| {
                Error::internal(format!(
                    "sandbox: attach pid {pid} to its control group: {error}"
                ))
            })?;
        Ok(Armed { plan: self })
    }
}

/// A group that is accounting for a running child.
#[derive(Debug)]
pub(crate) struct Armed {
    plan: Plan,
}

impl Armed {
    /// Whether a cap was exceeded.
    ///
    /// An `Err` means the accounting itself is gone (the group was removed, or a
    /// controller disappeared), which the caller treats as "cannot enforce" and
    /// kills the session rather than running it unguarded.
    pub(crate) fn check(&mut self) -> Result<Option<ExitReason>> {
        if self.plan.memory {
            let events = read(&self.plan.cgroup, "memory.events")?;
            if keyed_value(&events, "oom_kill").unwrap_or(0) > 0 {
                return Ok(Some(ExitReason::BudgetExceeded));
            }
        }
        if let Some(budget) = self.plan.cpu_budget_usec {
            let stat = read(&self.plan.cgroup, "cpu.stat")?;
            if keyed_value(&stat, "usage_usec").unwrap_or(0) > budget {
                return Ok(Some(ExitReason::BudgetExceeded));
            }
        }
        Ok(None)
    }

    /// Release the group. The process must already be gone (v2 refuses to remove
    /// a populated group, and a best-effort removal is the right behaviour here).
    pub(crate) fn close(self) {
        let _ = self.plan.cgroup.delete();
    }
}

/// Read one accounting file out of the control group.
fn read(cgroup: &Cgroup, name: &str) -> Result<String> {
    let path = accounting_path(cgroup, name)?;
    std::fs::read_to_string(&path).map_err(|error| {
        Error::not_available(format!(
            "sandbox: cannot account for the session: {}: {error}",
            path.display()
        ))
    })
}

/// The absolute path of one accounting file.
///
/// The controller owns the directory, so ask it rather than rebuilding the v2
/// layout by hand.
fn accounting_path(cgroup: &Cgroup, name: &str) -> Result<PathBuf> {
    let directory: Option<PathBuf> = if name.starts_with("memory") {
        let controller: Option<&MemController> = cgroup.controller_of::<MemController>();
        controller.map(|controller| controller.path().to_path_buf())
    } else {
        let controller: Option<&CpuController> = cgroup.controller_of::<CpuController>();
        controller.map(|controller| controller.path().to_path_buf())
    };
    directory.map(|dir| dir.join(name)).ok_or_else(|| {
        Error::not_available(format!(
            "sandbox: control group {:?} has no controller for {name}",
            cgroup.path()
        ))
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_probe_reports_what_it_could_really_do() {
        // On a host with a writable v2 hierarchy both caps are available; on one
        // without, both are absent. Never a guess in between.
        let support = Backend::probe().support();
        assert_eq!(
            support.memory, support.cpu,
            "one unified hierarchy either works for both or for neither"
        );
    }

    #[test]
    fn a_group_can_be_created_armed_and_removed_when_the_host_allows_it() {
        let support = Backend::probe().support();
        if !support.memory {
            eprintln!("skipping: cgroup v2 is not writable here");
            return;
        }
        let plan = plan("test-arm", Some(64 * 1024 * 1024), None).expect("plan");
        let mut armed = plan.arm(std::process::id()).expect("arm");
        assert!(armed.check().expect("check").is_none());
        // Put this test process back where it belongs before deleting the group.
        let path = armed.plan.cgroup.path().to_owned();
        let _ = armed
            .plan
            .cgroup
            .move_task_to_parent_by_tgid(CgroupPid::from(u64::from(std::process::id())));
        armed.close();
        assert!(!std::path::Path::new(&path).exists() || path.is_empty());
    }

    #[test]
    fn an_unknown_accounting_file_is_an_error_not_a_zero() {
        let support = Backend::probe().support();
        if !support.memory {
            eprintln!("skipping: cgroup v2 is not writable here");
            return;
        }
        let cgroup = CgroupBuilder::new("flowcraft/read-test")
            .build(cgroups_rs::fs::hierarchies::auto())
            .expect("cgroup");
        assert!(
            read(&cgroup, "memory.events").is_err(),
            "no memory controller"
        );
        let _ = cgroup.delete();
    }
}
