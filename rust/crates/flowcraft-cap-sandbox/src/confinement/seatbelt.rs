//! The seatbelt profile and argv plan.
//!
//! `sandbox-exec` runs the child on the host filesystem, so the only boundary is
//! the SBPL profile. It is built in three layers, in this order (SBPL applies
//! the **last** matching rule):
//!
//! 1. `(allow default)` — reads and process execution stay open so a local agent
//!    can reach the host toolchain;
//! 2. `(deny file-write*)` — unconditional: the default mode confines writes
//!    too, which is what lets this backend claim a filesystem boundary at all;
//! 3. one `(allow file-write* (subpath …))` per writable path, so a write lands
//!    only inside the runner root, the explicit exceptions, and `/dev/null`.
//!
//! A read-only call simply omits the root from layer 3; the exceptions stay,
//! because a call narrows the root and never widens what it was granted.

use std::path::Path;

use flowcraft_error::{Error, Result};

use super::{Plan, SandboxConfig};
use crate::contract::{ExecOptions, NetMode, SessionSpec, WriteMode};

/// Build the argv that runs `spec` under the profile.
pub(crate) fn plan(config: &SandboxConfig, spec: &SessionSpec, workdir: &Path) -> Result<Plan> {
    if spec.opts.net.mode.uses_proxy() {
        return Err(Error::not_available(format!(
            "sandbox/seatbelt: net mode {} needs the enforcement proxy",
            spec.opts.net.mode.as_str()
        )));
    }
    let mut argv = vec![
        config.binary().to_owned(),
        "-p".to_owned(),
        profile(config, &spec.opts),
        "--".to_owned(),
    ];
    argv.extend(spec.argv.iter().cloned());
    Ok(Plan {
        argv,
        // sandbox-exec inherits the working directory; the inner runner sets it.
        cwd: Some(workdir.to_path_buf()),
        env: Vec::new(),
    })
}

/// Render the SBPL profile for one call.
#[must_use]
pub(crate) fn profile(config: &SandboxConfig, opts: &ExecOptions) -> String {
    let mut profile = String::from("(version 1)\n(allow default)\n(deny file-write*)\n");
    for path in writable_paths(config, opts.write) {
        profile.push_str(&format!(
            "(allow file-write* (subpath {}))\n",
            sbpl_string(&path.display().to_string())
        ));
    }
    // A shell that redirects to /dev/null must keep working.
    profile.push_str("(allow file-write* (literal \"/dev/null\"))\n");
    if opts.net.mode == NetMode::DenyAll {
        profile.push_str("(deny network*)\n");
    }
    profile
}

/// The paths a call may write to: the root (unless pinned or narrowed) plus the
/// explicit exceptions.
#[must_use]
fn writable_paths(config: &SandboxConfig, write: WriteMode) -> Vec<&Path> {
    let mut paths = Vec::new();
    if !config.readonly_root() && write != WriteMode::ReadOnly {
        paths.push(config.root());
    }
    paths.extend(config.writable().iter().map(|path| path.as_path()));
    paths
}

/// Quote a path for an SBPL string literal.
///
/// Both `\` and `"` are escaped. Control characters are rejected when the path is
/// resolved (a newline in a profile is an injection, not a filename), so this
/// stays a pure escape.
fn sbpl_string(raw: &str) -> String {
    let mut out = String::with_capacity(raw.len() + 2);
    out.push('"');
    for ch in raw.chars() {
        if ch == '\\' || ch == '"' {
            out.push('\\');
        }
        out.push(ch);
    }
    out.push('"');
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::confinement::{Sandbox, SandboxConfig};

    fn sandbox_config(root: &Path) -> SandboxConfig {
        SandboxConfig::new(Sandbox::Seatbelt, root)
    }

    fn real(root: &Path) -> std::path::PathBuf {
        std::fs::canonicalize(root).expect("canonical")
    }

    fn spec(argv: &[&str]) -> SessionSpec {
        SessionSpec::new(argv.iter().map(|arg| (*arg).to_owned()).collect())
    }

    #[test]
    fn writes_are_denied_outside_the_root_by_default() {
        let dir = tempfile::tempdir().expect("tempdir");
        let profile = profile(&sandbox_config(dir.path()), &ExecOptions::default());
        assert!(profile.contains("(deny file-write*)"), "{profile}");
        assert!(
            profile.contains(&format!(
                "(allow file-write* (subpath {}))",
                sbpl_string(&real(dir.path()).display().to_string())
            )),
            "{profile}"
        );
        assert!(
            profile.find("(deny file-write*)").expect("deny")
                < profile.find("(allow file-write*").expect("allow"),
            "the deny must come first so the allowance wins: {profile}"
        );
    }

    #[test]
    fn a_read_only_call_drops_the_root_but_keeps_the_exceptions() {
        let dir = tempfile::tempdir().expect("tempdir");
        std::fs::create_dir(dir.path().join("out")).expect("out");
        let out = real(&dir.path().join("out"));
        let config = sandbox_config(dir.path()).with_writable(vec![out.clone()]);
        let opts = ExecOptions {
            write: WriteMode::ReadOnly,
            ..ExecOptions::default()
        };
        let profile = profile(&config, &opts);
        let root_subpath = format!(
            "(subpath {})",
            sbpl_string(&real(dir.path()).display().to_string())
        );
        assert!(
            !profile.contains(&root_subpath),
            "the root must lose its allowance: {profile}"
        );
        assert!(
            profile.contains(&format!(
                "(subpath {})",
                sbpl_string(&out.display().to_string())
            )),
            "an explicit exception survives: {profile}"
        );
    }

    #[test]
    fn a_pinned_root_stays_read_only_for_every_call() {
        let dir = tempfile::tempdir().expect("tempdir");
        let config = sandbox_config(dir.path()).with_readonly_root();
        let profile = profile(&config, &ExecOptions::default());
        assert!(
            !profile.contains("(subpath"),
            "no path may be writable: {profile}"
        );
    }

    #[test]
    fn deny_all_denies_the_network() {
        let dir = tempfile::tempdir().expect("tempdir");
        let opts = ExecOptions {
            net: crate::contract::NetPolicy {
                mode: NetMode::DenyAll,
                ..crate::contract::NetPolicy::default()
            },
            ..ExecOptions::default()
        };
        assert!(profile(&sandbox_config(dir.path()), &opts).contains("(deny network*)"));
    }

    #[test]
    fn the_profile_is_the_third_argument() {
        let dir = tempfile::tempdir().expect("tempdir");
        let plan = plan(&sandbox_config(dir.path()), &spec(&["true"]), dir.path()).expect("plan");
        assert_eq!(plan.argv[1], "-p");
        assert!(
            plan.argv[2].starts_with("(version 1)"),
            "{:?}",
            plan.argv[2]
        );
        assert_eq!(plan.argv[3], "--");
        assert_eq!(plan.argv[4], "true");
        assert_eq!(plan.cwd.as_deref(), Some(dir.path()));
    }

    #[test]
    fn a_quote_in_a_path_cannot_break_out_of_the_literal() {
        assert_eq!(sbpl_string("/tmp/a\"b"), "\"/tmp/a\\\"b\"");
        assert_eq!(sbpl_string("/tmp/a\\b"), "\"/tmp/a\\\\b\"");
    }

    #[test]
    fn the_proxy_modes_are_not_claimed_yet() {
        let dir = tempfile::tempdir().expect("tempdir");
        let mut request = spec(&["true"]);
        request.opts.net.mode = NetMode::Proxy;
        let error = plan(&sandbox_config(dir.path()), &request, dir.path()).expect_err("no proxy");
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::NotAvailable);
    }
}
