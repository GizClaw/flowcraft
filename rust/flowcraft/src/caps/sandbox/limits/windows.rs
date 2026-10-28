//! The Windows caps.
//!
//! Enforcement is not here: a job object's memory ceiling and cpu budget are
//! installed when the session's job is created and read back from its
//! accounting, so this backend only *declares* what the host can enforce and
//! refuses what it cannot. That is the same split as the unix one — the claims
//! come from a real probe, not from optimism.

#![allow(dead_code)]

use crate::error::Result;

use super::Support;
use crate::caps::sandbox::contract::ExitReason;
use crate::caps::sandbox::session::job::SessionJob;

/// The backend of this host.
#[derive(Debug)]
pub(crate) struct Backend {
    support: Support,
}

impl Backend {
    /// Probe: creating a job is what everything else depends on.
    pub(crate) fn probe() -> Self {
        Self {
            support: Support {
                memory: SessionJob::probe(),
                cpu: SessionJob::probe(),
            },
        }
    }

    pub(crate) fn support(&self) -> Support {
        self.support
    }
}

/// No plan: the session's own job object carries the limits, so there is nothing
/// a supervisor has to arm or poll.
#[derive(Debug)]
pub(crate) struct Plan;

/// Never constructed.
#[derive(Debug)]
pub(crate) struct Armed;

pub(crate) fn plan(_id: &str, _memory: Option<u64>, _cpu: Option<u64>) -> Result<Plan> {
    Ok(Plan)
}

impl Plan {
    pub(crate) fn arm(self, _pid: u32) -> Result<Armed> {
        Ok(Armed)
    }
}

impl Armed {
    pub(crate) fn check(&mut self) -> Result<Option<ExitReason>> {
        Ok(None)
    }

    pub(crate) fn close(self) {}
}
