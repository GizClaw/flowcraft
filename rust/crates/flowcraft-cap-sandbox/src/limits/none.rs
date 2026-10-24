//! The platform shim for hosts without cgroups.
//!
//! It exists so the signatures in [`super`] are uniform: on every platform a
//! plan can be made and an armed group can be checked, and the difference is
//! confined to what the probe reports. `plan` is unreachable here because
//! [`super::Manager::check`] rejects every cap request first — the dead-code
//! allowance is what keeps this shim free of `cfg` branches in the caller.

#![allow(dead_code)]

use flowcraft_error::{Error, Result};

use super::Support;
use crate::contract::ExitReason;

/// There is no backend here.
#[derive(Debug)]
pub(crate) struct Backend;

impl Backend {
    pub(crate) fn probe() -> Self {
        Self
    }

    pub(crate) fn support(&self) -> Support {
        Support::default()
    }
}

/// Never constructed: `Manager::check` refuses first.
#[derive(Debug)]
pub(crate) struct Plan;

/// Never constructed.
#[derive(Debug)]
pub(crate) struct Armed;

pub(crate) fn plan(_id: &str, _memory: Option<u64>, _cpu: Option<u64>) -> Result<Plan> {
    Err(Error::not_available(
        "sandbox: resource limits require cgroups, which only Linux has",
    ))
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
