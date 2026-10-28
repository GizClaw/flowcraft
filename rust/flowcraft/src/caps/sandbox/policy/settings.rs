//! The policy chain, as a deployment describes it.
//!
//! Everything in [`super`] is composable in code — `Chain::of([...])`, a
//! [`LocalPolicy`](super::approval::LocalPolicy), a hand-written layer. A
//! deployment cannot do that: it has a document. This module is the translation,
//! and it is deliberately narrow — the three things an operator actually asks for
//! (which commands may run, what is worth flagging, and whether a proven read-only
//! call may answer for itself) and nothing else.
//!
//! The order of the resulting chain is the one [`compose_local`] documents, with
//! hardening first: `defaults → harden_read_only → approval → allow_commands →
//! backend`. Hardening runs before approval so the approver — and the automatic one
//! — see the spec that will actually be executed.

use std::path::Path;
use std::sync::Arc;

use serde::Deserialize;

use crate::caps::sandbox::contract::HostPolicy;
use crate::caps::sandbox::policy::approval::{
    LocalPolicy, Predicate, command_patterns, compose_local, interactive, net_non_default,
    workdir_outside_root,
};
use crate::caps::sandbox::policy::{Allowlist, RunnerRef};
use crate::error::{Error, Result};

/// The tripwires a document may name.
const FLAGS: [&str; 3] = ["net_non_default", "workdir_outside_root", "interactive"];

/// A deployment's policy chain. Absent means "no chain": every call goes straight
/// to the backend, which is what every deployment did before this existed.
#[derive(Debug, Clone, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PolicySettings {
    /// A hard gate: only these commands may run. A rule containing `/` is a path
    /// and is matched against the resolved programme; a bare rule names a host
    /// programme (see [`HostPolicy`]).
    #[serde(default)]
    pub allow_commands: Option<Vec<String>>,
    /// Token-prefix rules that skip the tripwires — `git status`, `git log -n`.
    /// Anything not listed still runs, but is flagged.
    #[serde(default)]
    pub allow: Vec<String>,
    /// Which tripwires to install. Unknown names are a build error, because a
    /// typo would silently install no tripwire at all.
    #[serde(default)]
    pub flag: Vec<String>,
    /// Command names that are always worth a second look (`rm`, `dd`, `git`).
    #[serde(default)]
    pub flag_commands: Vec<String>,
    /// Let a call the classifier proved read-only, and that asked for a read-only
    /// write mode, answer for itself instead of being refused.
    ///
    /// Refused, not deferred: a provider has no human to ask, so without this the
    /// chain's answer to anything flagged is a denial. That is the fail-closed
    /// direction, and it is why the setting exists rather than being the default.
    #[serde(default)]
    pub approve_read_only: bool,
}

impl PolicySettings {
    /// Whether this document asks for anything at all.
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.allow_commands.is_none()
            && self.allow.is_empty()
            && self.flag.is_empty()
            && self.flag_commands.is_empty()
            && !self.approve_read_only
    }

    /// Check the names, so a typo fails the build instead of installing nothing.
    pub fn validate(&self) -> Result<()> {
        for (index, name) in self.flag.iter().enumerate() {
            let name = name.trim();
            if !FLAGS.contains(&name) {
                return Err(Error::validation(format!(
                    "sandbox: policy.flag[{index}] {name:?} is not one of {}",
                    FLAGS.join(", ")
                )));
            }
        }
        for (index, command) in self.flag_commands.iter().enumerate() {
            if command.trim().is_empty() {
                return Err(Error::validation(format!(
                    "sandbox: policy.flag_commands[{index}] is empty"
                )));
            }
        }
        if let Some(commands) = &self.allow_commands {
            for (index, command) in commands.iter().enumerate() {
                if command.trim().is_empty() {
                    return Err(Error::validation(format!(
                        "sandbox: policy.allow_commands[{index}] is empty"
                    )));
                }
            }
        }
        // Parsed here rather than at the first call: an invalid rule is a document
        // bug, and the build is the only moment it can be reported clearly.
        Allowlist::parse(self.allow.iter().cloned())?;
        Ok(())
    }

    /// Wrap `backend` in the chain this document describes.
    ///
    /// `root` is the runner's filesystem boundary, for the one tripwire that needs
    /// it; a document that asks for `workdir_outside_root` without a root is a
    /// build error rather than a tripwire that never fires.
    pub fn around(
        &self,
        backend: RunnerRef,
        host: HostPolicy,
        root: Option<&Path>,
    ) -> Result<RunnerRef> {
        self.validate()?;
        if self.is_empty() {
            return Ok(backend);
        }
        let mut predicates: Vec<Arc<dyn Predicate>> = Vec::new();
        for name in &self.flag {
            match name.trim() {
                "net_non_default" => predicates.push(net_non_default()),
                "interactive" => predicates.push(interactive()),
                "workdir_outside_root" => {
                    let root = root.ok_or_else(|| {
                        Error::validation(
                            "sandbox: policy.flag = workdir_outside_root needs a runner root",
                        )
                    })?;
                    predicates.push(workdir_outside_root(root.to_path_buf()));
                }
                other => {
                    return Err(Error::validation(format!(
                        "sandbox: policy.flag {other:?} is not one of {}",
                        FLAGS.join(", ")
                    )));
                }
            }
        }
        if !self.flag_commands.is_empty() {
            predicates.push(command_patterns(self.flag_commands.clone())?);
        }
        let allowlist = if self.allow.is_empty() {
            None
        } else {
            Some(Arc::new(Allowlist::parse(self.allow.clone())?))
        };
        Ok(compose_local(
            backend,
            LocalPolicy {
                allowed_commands: self.allow_commands.clone(),
                allowlist,
                // No approver: a provider has no human to ask, so the chain's
                // answer to a flagged call it cannot answer for itself is a denial.
                approver: None,
                predicates,
                host,
                auto_approve_read_only: self.approve_read_only,
                ..LocalPolicy::default()
            },
        ))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn settings(json: serde_json::Value) -> PolicySettings {
        serde_json::from_value(json).expect("settings")
    }

    #[test]
    fn an_unknown_tripwire_is_a_build_error() {
        let error = settings(serde_json::json!({"flag": ["net_non_defualt"]}))
            .validate()
            .expect_err("typo");
        assert!(error.to_string().contains("net_non_default"), "{error}");
        assert!(
            settings(serde_json::json!({"flag": ["net_non_default", "interactive"]}))
                .validate()
                .is_ok()
        );
    }

    #[test]
    fn an_empty_command_name_is_a_build_error() {
        assert!(
            settings(serde_json::json!({"allow_commands": [""]}))
                .validate()
                .is_err()
        );
        assert!(
            settings(serde_json::json!({"flag_commands": [" "]}))
                .validate()
                .is_err()
        );
    }

    /// A document that names no chain installs none — the backend is handed back
    /// unchanged, which is what every deployment did before this existed.
    #[test]
    fn an_empty_document_installs_nothing() {
        let policy = PolicySettings::default();
        assert!(policy.is_empty());
        let backend: RunnerRef = Arc::new(crate::caps::sandbox::local::LocalRunner::new());
        let wrapped = policy
            .around(backend.clone(), HostPolicy::default(), None)
            .expect("no chain");
        assert!(
            Arc::ptr_eq(&wrapped, &backend),
            "an empty document must hand the backend back, not wrap it"
        );
    }

    /// The gate, through the document: a command the deployment did not list never
    /// reaches the backend.
    #[cfg(unix)]
    #[tokio::test]
    async fn a_document_that_names_commands_gates_them() {
        use crate::caps::sandbox::contract::SessionSpec;

        let dir = tempfile::tempdir().expect("tempdir");
        let sh = program(dir.path(), "sh");
        let rm = program(dir.path(), "rm");
        let host = host_of(dir.path());
        let policy = settings(serde_json::json!({"allow_commands": ["sh"]}));
        let chain = policy
            .around(
                Arc::new(crate::caps::sandbox::policy::stub::Sink::default()),
                host,
                Some(dir.path()),
            )
            .expect("chain");

        // Allowed: it reaches the backend, which is a recording stub and therefore
        // refuses to spawn — the point is *which* error comes back.
        let allowed = chain
            .start(SessionSpec::new(vec![sh.display().to_string()]))
            .await
            .expect_err("the stub does not spawn");
        assert_ne!(
            allowed.kind(),
            crate::error::ErrorKind::PolicyDenied,
            "a listed command must pass the gate"
        );

        let denied = chain
            .start(SessionSpec::new(vec![rm.display().to_string()]))
            .await
            .expect_err("gated");
        assert_eq!(denied.kind(), crate::error::ErrorKind::PolicyDenied);
    }

    /// And the read-only answer, through the document.
    #[cfg(unix)]
    #[tokio::test]
    async fn a_document_may_let_a_proven_read_only_call_answer_for_itself() {
        use crate::caps::sandbox::contract::{NetMode, SessionSpec, WriteMode};

        let dir = tempfile::tempdir().expect("tempdir");
        let ls = program(dir.path(), "ls");
        let rm = program(dir.path(), "rm");
        let host = host_of(dir.path());
        let policy = settings(serde_json::json!({
            "flag": ["net_non_default"],
            "approve_read_only": true,
        }));
        let chain = policy
            .around(
                Arc::new(crate::caps::sandbox::policy::stub::Sink::default()),
                host,
                Some(dir.path()),
            )
            .expect("chain");

        let mut read_only = SessionSpec::new(vec![ls.display().to_string()]);
        read_only.opts.write = WriteMode::ReadOnly;
        read_only.opts.net.mode = NetMode::DenyAll;
        let reached = chain
            .start(read_only)
            .await
            .expect_err("the stub does not spawn");
        assert_ne!(
            reached.kind(),
            crate::error::ErrorKind::PolicyDenied,
            "a proven read-only call answers for itself"
        );

        // A writer is flagged and there is nobody to ask, so the chain refuses it.
        let mut writer = SessionSpec::new(vec![rm.display().to_string()]);
        writer.opts.write = WriteMode::ReadOnly;
        writer.opts.net.mode = NetMode::DenyAll;
        let denied = chain.start(writer).await.expect_err("denied");
        assert_eq!(denied.kind(), crate::error::ErrorKind::PolicyDenied);
    }

    /// A host policy that resolves bare names against `dir` only.
    #[cfg(unix)]
    fn host_of(dir: &Path) -> HostPolicy {
        HostPolicy {
            path_dirs: vec![std::fs::canonicalize(dir).expect("canonicalize")],
        }
    }

    /// An executable file, so the resolver has something to find.
    #[cfg(unix)]
    fn program(dir: &Path, name: &str) -> std::path::PathBuf {
        use std::os::unix::fs::PermissionsExt as _;

        let path = dir.join(name);
        std::fs::write(
            &path,
            "#!/bin/sh
exit 0
",
        )
        .expect("write");
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o755)).expect("chmod");
        std::fs::canonicalize(path).expect("canonicalize")
    }

    #[test]
    fn a_workdir_tripwire_needs_a_root_to_compare_against() {
        let policy = settings(serde_json::json!({"flag": ["workdir_outside_root"]}));
        let backend: RunnerRef = Arc::new(crate::caps::sandbox::local::LocalRunner::new());
        let error = policy
            .around(backend, HostPolicy::default(), None)
            .expect_err("no root");
        assert!(error.to_string().contains("needs a runner root"), "{error}");
    }
}
