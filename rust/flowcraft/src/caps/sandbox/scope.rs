//! Grant scope: what a session may run.

use std::time::Duration;

use crate::error::Result;
use serde_json::Value;

use crate::caps::sandbox::codes::{self, Code};
use crate::caps::sandbox::policy::PlannedCommand;

/// Limits signed into a `sandbox.Runner/v1` grant.
#[derive(Debug, Clone, Default, serde::Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Scope {
    /// Allowed `argv[0]` values. Empty means "no command restriction".
    #[serde(default)]
    pub commands: Vec<String>,
    /// Upper bound on a session timeout.
    #[serde(default)]
    pub max_timeout_secs: Option<u64>,
    /// Upper bound on buffered output.
    #[serde(default)]
    pub max_output_bytes: Option<u64>,
}

impl Scope {
    /// Decode a scope. A missing scope is the default scope.
    pub fn parse(value: &Value) -> Result<Self> {
        if value.is_null() {
            return Ok(Self::default());
        }
        crate::resource::settings(value)
    }

    /// Whether this grant may run the planned command.
    ///
    /// The comparison is against what will be exec'd, never against the
    /// `argv[0]` the caller spelt: the caller owns the child's `PATH`, so
    /// judging the spelling would let `argv[0] = "git"` run whatever `PATH`
    /// resolves to. A rule containing `/` is a pinned path (canonicalized, so
    /// `..` and symlink spellings of the same file agree); a bare rule names a
    /// programme the host's own PATH resolves.
    pub fn allows_command(&self, planned: &PlannedCommand) -> Result<()> {
        if self.commands.is_empty() {
            return Ok(());
        }
        let Some(program) = planned.program() else {
            return Err(codes::forbidden(
                Code::CommandNotAllowed,
                format!(
                    "sandbox: {:?} does not resolve to an executable file, so it is outside the grant's command allow-list",
                    planned.argv0
                ),
            ));
        };
        if self.commands.iter().any(|rule| planned.names(rule)) {
            return Ok(());
        }
        Err(codes::forbidden(
            Code::CommandNotAllowed,
            format!(
                "sandbox: {:?} ({}) is outside the grant's command allow-list",
                planned.argv0,
                program.display()
            ),
        ))
    }

    /// Clamp a requested timeout to the grant's ceiling.
    pub fn timeout(&self, requested: Option<Duration>) -> Result<Option<Duration>> {
        let Some(max) = self.max_timeout_secs else {
            return Ok(requested);
        };
        let max = Duration::from_secs(max);
        match requested {
            Some(requested) if requested > max => Err(codes::forbidden(
                Code::TimeoutExceedsGrant,
                format!("sandbox: timeout {requested:?} exceeds the grant's {max:?}"),
            )),
            Some(requested) => Ok(Some(requested)),
            None => Ok(Some(max)),
        }
    }

    /// The output bound to apply, taking the stricter of grant and request.
    #[must_use]
    pub fn output_limit(&self, requested: Option<u64>) -> Option<u64> {
        match (self.max_output_bytes, requested) {
            (Some(grant), Some(requested)) => Some(grant.min(requested)),
            (Some(grant), None) => Some(grant),
            (None, requested) => requested,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::caps::sandbox::contract::SessionSpec;
    use crate::caps::sandbox::policy::HostPolicy;
    use crate::caps::sandbox::policy::plan::{Resolution, plan};
    use std::path::{Path, PathBuf};

    fn planned(program: PathBuf, path_dir: Option<PathBuf>) -> PlannedCommand {
        PlannedCommand {
            argv0: program.display().to_string(),
            argv: vec![program.display().to_string()],
            resolution: Resolution::Path { program, path_dir },
            env: Vec::new(),
            injected: Vec::new(),
        }
    }

    // Every caller is a `#[cfg(unix)]` test: they build a real programme tree and
    // ask the host policy to resolve it.
    #[cfg_attr(not(unix), allow(dead_code))]
    fn host(dir: &Path) -> HostPolicy {
        HostPolicy {
            path_dirs: vec![std::fs::canonicalize(dir).expect("canonicalize")],
        }
    }

    #[cfg(unix)]
    fn program(dir: &Path, name: &str) -> PathBuf {
        use std::os::unix::fs::PermissionsExt;
        let path = dir.join(name);
        std::fs::write(&path, "#!/bin/sh\nexit 0\n").expect("write");
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o755)).expect("chmod");
        std::fs::canonicalize(path).expect("canonicalize")
    }

    /// The programmes are built in a temporary directory rather than named by
    /// absolute path: a grant compares canonical paths, and `/usr/bin/sh` is a
    /// symlink to `dash` on Debian while it does not exist at all on macOS, so a
    /// distribution's layout would decide the answer.
    #[cfg(unix)]
    #[test]
    fn commands_are_an_allow_list() {
        let dir = tempfile::tempdir().expect("tempdir");
        let host = host(dir.path());
        let sh = program(dir.path(), "sh");
        let rg = program(dir.path(), "rg");
        let rm = program(dir.path(), "rm");
        let scope = Scope::parse(&serde_json::json!({"commands": [
            sh.display().to_string(),
            rg.display().to_string(),
        ]}))
        .expect("scope");
        scope
            .allows_command(&plan(
                &SessionSpec::new(vec![sh.display().to_string()]),
                &host,
            ))
            .expect("an allowed programme");
        assert_eq!(
            scope
                .allows_command(&plan(
                    &SessionSpec::new(vec![rm.display().to_string()]),
                    &host,
                ))
                .unwrap_err()
                .kind(),
            crate::error::ErrorKind::Forbidden
        );
    }

    #[test]
    fn an_empty_allow_list_permits_anything() {
        let scope = Scope::default();
        scope
            .allows_command(&planned(PathBuf::from("/bin/rm"), None))
            .unwrap();
    }

    #[test]
    fn an_unresolved_programme_is_refused() {
        let scope = Scope::parse(&serde_json::json!({"commands": ["git"]})).unwrap();
        let planned = plan(
            &SessionSpec::new(vec!["definitely-not-a-sandbox-programme".to_owned()]),
            &HostPolicy::default(),
        );
        assert_eq!(
            scope.allows_command(&planned).unwrap_err().kind(),
            crate::error::ErrorKind::Forbidden
        );
    }

    #[cfg(unix)]
    #[test]
    fn a_grant_compares_the_canonical_path() {
        let dir = tempfile::tempdir().expect("tempdir");
        let real = program(dir.path(), "git");
        // A second spelling of the same file: `..` back into the directory.
        let spelled = dir
            .path()
            .join("..")
            .join(dir.path().file_name().expect("name"))
            .join("git");
        let grant = Scope::parse(&serde_json::json!({
            "commands": [spelled.display().to_string()]
        }))
        .unwrap();
        // The command is resolved from the host's PATH; the rule is spelt
        // differently. Both canonicalize to one file, so they agree.
        let mut spec = SessionSpec::new(vec!["git".to_owned()]);
        spec.opts
            .env
            .inject
            .insert("PATH".to_owned(), "/tmp/evil".to_owned());
        let planned = plan(&spec, &host(dir.path()));
        assert_eq!(planned.program(), Some(real.as_path()));
        grant.allows_command(&planned).unwrap();
    }

    #[cfg(unix)]
    #[test]
    fn a_bare_rule_does_not_follow_a_smuggled_binary() {
        let host_dir = tempfile::tempdir().expect("tempdir");
        let evil = tempfile::tempdir().expect("tempdir");
        program(host_dir.path(), "git");
        let smuggled = program(evil.path(), "git");
        let grant = Scope::parse(&serde_json::json!({"commands": ["git"]})).unwrap();
        // The caller's PATH is /tmp/evil, so a string comparison would approve
        // the smuggled binary; the plan resolves the host's `git` instead.
        let mut spec = SessionSpec::new(vec!["git".to_owned()]);
        spec.opts
            .env
            .inject
            .insert("PATH".to_owned(), evil.path().display().to_string());
        let resolved = plan(&spec, &host(host_dir.path()));
        assert_ne!(resolved.program(), Some(smuggled.as_path()));
        grant.allows_command(&resolved).unwrap();
        // A path the caller spells itself is another matter: a bare rule does
        // not follow it.
        let spelled = SessionSpec::new(vec![smuggled.display().to_string()]);
        let resolved = plan(&spelled, &host(host_dir.path()));
        assert_eq!(
            grant.allows_command(&resolved).unwrap_err().kind(),
            crate::error::ErrorKind::Forbidden
        );
    }

    #[test]
    fn timeouts_are_capped_not_silently_shortened() {
        let scope = Scope::parse(&serde_json::json!({"max_timeout_secs": 60})).unwrap();
        assert_eq!(scope.timeout(None).unwrap(), Some(Duration::from_secs(60)));
        assert_eq!(
            scope.timeout(Some(Duration::from_secs(30))).unwrap(),
            Some(Duration::from_secs(30))
        );
        assert_eq!(
            scope
                .timeout(Some(Duration::from_secs(120)))
                .unwrap_err()
                .kind(),
            crate::error::ErrorKind::Forbidden
        );
    }

    #[test]
    fn the_output_limit_is_the_stricter_of_the_two() {
        let scope = Scope::parse(&serde_json::json!({"max_output_bytes": 1024})).unwrap();
        assert_eq!(scope.output_limit(None), Some(1024));
        assert_eq!(scope.output_limit(Some(2048)), Some(1024));
        assert_eq!(scope.output_limit(Some(16)), Some(16));
    }

    #[test]
    fn unknown_fields_are_rejected() {
        assert!(Scope::parse(&serde_json::json!({"commands": [], "root": "/"})).is_err());
    }
}
