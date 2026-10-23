//! The bubblewrap argv plan.
//!
//! `bwrap` is the only backend that builds a real mount boundary, so this module
//! is where "writes are confined" becomes true: the host root is bound
//! read-only, the runner root (and every explicit writable path) is bound
//! read-write, and nothing else is writable.
//!
//! Two orderings are load-bearing rather than stylistic:
//!
//! 1. caller-supplied `extra_flags` come **first**, so every policy flag we emit
//!    afterwards wins over an escape hatch;
//! 2. the private `--tmpfs /run` comes **after** the filesystem binds and
//!    **before** anything bound into `/run` (the enforcement proxy socket lands
//!    there in the network phase). Reversed, the host's whole `/run` tree —
//!    docker.sock, dbus, systemd — would be handed to the sandbox.

use std::path::Path;

use flowcraft_error::{Error, Result};

use super::{Plan, SandboxConfig};
use crate::contract::{NetMode, SessionSpec};

/// Every `bwrap` option that can weaken a policy dimension this backend
/// promises to enforce (filesystem bounds, env allow-list, net posture,
/// namespace isolation, workdir confinement, seccomp, capability drops) or
/// smuggle extra options past validation (`--args`).
///
/// This table is a security property, not a convenience: `WithExtraFlags` is an
/// escape hatch for diagnostics, never for policy.
const BANNED: &[&str] = &[
    // filesystem / mount
    "--ro-bind",
    "--ro-bind-try",
    "--bind",
    "--bind-try",
    "--dev-bind",
    "--dev-bind-try",
    "--bind-fd",
    "--ro-bind-fd",
    "--remount-ro",
    "--overlay-src",
    "--overlay",
    "--tmp-overlay",
    "--ro-overlay",
    "--proc",
    "--dev",
    "--tmpfs",
    "--mqueue",
    "--dir",
    "--file",
    "--bind-data",
    "--ro-bind-data",
    "--symlink",
    "--chmod",
    "--perms",
    // environment
    "--clearenv",
    "--setenv",
    "--unsetenv",
    // namespaces / network
    "--unshare-all",
    "--share-net",
    "--unshare-user",
    "--unshare-user-try",
    "--unshare-ipc",
    "--unshare-pid",
    "--unshare-net",
    "--unshare-uts",
    "--unshare-cgroup",
    "--unshare-cgroup-try",
    "--disable-userns",
    "--assert-userns-disabled",
    "--userns",
    "--userns2",
    "--pidns",
    // workdir / identity / seccomp / capabilities / session
    "--chdir",
    "--uid",
    "--gid",
    "--hostname",
    "--seccomp",
    "--add-seccomp-fd",
    "--cap-add",
    "--cap-drop",
    "--new-session",
    // argument smuggling
    "--args",
];

/// Reject any extra flag that could weaken the boundary.
pub(crate) fn validate_extra_flags(flags: &[String]) -> Result<()> {
    for flag in flags {
        let name = flag.split('=').next().unwrap_or(flag);
        if BANNED.contains(&name) {
            return Err(Error::validation(format!(
                "sandbox/bwrap: extra_flags entry {flag:?} can weaken the policy"
            )));
        }
    }
    Ok(())
}

/// Build the argv that runs `spec` inside the sandbox.
pub(crate) fn plan(config: &SandboxConfig, spec: &SessionSpec, workdir: &Path) -> Result<Plan> {
    let mut argv = vec![config.binary().to_owned()];
    // Extras first: every flag below is emitted after them on purpose.
    argv.extend(config.extra_flags().iter().cloned());

    argv.push("--die-with-parent".to_owned());
    // A fresh pid namespace: the child sees itself as pid 1 and cannot observe
    // or signal host processes.
    argv.push("--unshare-pid".to_owned());

    argv.push("--ro-bind".to_owned());
    argv.push("/".to_owned());
    argv.push("/".to_owned());
    // A private writable /tmp, mounted before the writable binds so a runner
    // root that happens to live under /tmp stays visible inside it.
    argv.push("--tmpfs".to_owned());
    argv.push("/tmp".to_owned());

    // The runner root: read-write unless pinned, or unless this call asked for
    // read-only.
    let root_read_only =
        config.readonly_root() || spec.opts.write == crate::contract::WriteMode::ReadOnly;
    argv.push(
        if root_read_only {
            "--ro-bind"
        } else {
            "--bind"
        }
        .to_owned(),
    );
    push_path(&mut argv, config.root());
    push_path(&mut argv, config.root());

    // Explicit exceptions stay writable even for a read-only call: the caller
    // narrows the root, not the exceptions it was granted.
    for path in config.writable() {
        argv.push("--bind".to_owned());
        push_path(&mut argv, path);
        push_path(&mut argv, path);
    }

    argv.push("--proc".to_owned());
    argv.push("/proc".to_owned());
    argv.push("--dev".to_owned());
    argv.push("/dev".to_owned());

    match spec.opts.net.mode {
        NetMode::Default => argv.push("--share-net".to_owned()),
        NetMode::DenyAll => {
            argv.push("--unshare-net".to_owned());
            // Hide the host's /run (unix sockets) — `--unshare-net` does not.
            argv.push("--tmpfs".to_owned());
            argv.push(super::RUN_DIR.to_owned());
        }
        NetMode::AllowList | NetMode::Proxy => {
            return Err(Error::not_available(format!(
                "sandbox/bwrap: net mode {} needs the enforcement proxy",
                spec.opts.net.mode.as_str()
            )));
        }
    }

    argv.push("--chdir".to_owned());
    push_path(&mut argv, workdir);
    argv.push("--".to_owned());
    argv.extend(spec.argv.iter().cloned());
    Ok(Plan {
        argv,
        cwd: None,
        env: Vec::new(),
    })
}

fn push_path(argv: &mut Vec<String>, path: &Path) {
    argv.push(path.display().to_string());
}

/// The bind list a caller can inspect in tests.
#[cfg(test)]
#[must_use]
pub(crate) fn writable_binds(config: &SandboxConfig) -> Vec<std::path::PathBuf> {
    let mut paths = Vec::new();
    if !config.readonly_root() {
        paths.push(config.root().to_path_buf());
    }
    paths.extend(config.writable().iter().cloned());
    paths
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::confinement::{Sandbox, SandboxConfig};
    use crate::contract::{ExecOptions, WriteMode};

    fn sandbox_config(root: &Path) -> SandboxConfig {
        SandboxConfig::new(Sandbox::Bwrap, root)
    }

    /// The root as the configuration sees it (canonical).
    fn real(root: &Path) -> std::path::PathBuf {
        std::fs::canonicalize(root).expect("canonical")
    }

    fn spec(argv: &[&str]) -> SessionSpec {
        SessionSpec::new(argv.iter().map(|arg| (*arg).to_owned()).collect())
    }

    #[test]
    fn the_root_is_bound_writable_and_the_host_root_is_not() {
        let dir = tempfile::tempdir().expect("tempdir");
        let config = sandbox_config(dir.path());
        let plan = plan(&config, &spec(&["true"]), dir.path()).expect("plan");
        let joined = plan.argv.join(" ");
        assert!(joined.contains("--ro-bind / /"), "{joined}");
        let root = real(dir.path());
        assert!(
            joined.contains(&format!("--bind {} {}", root.display(), root.display())),
            "{joined}"
        );
        assert_eq!(plan.argv.last().expect("command"), "true");
        let tail = plan
            .argv
            .split(|arg| arg == "--")
            .nth(1)
            .expect("command tail");
        assert_eq!(tail, ["true"]);
    }

    #[test]
    fn extra_flags_come_before_every_policy_flag() {
        let dir = tempfile::tempdir().expect("tempdir");
        assert!(
            sandbox_config(dir.path())
                .with_extra_flags(vec!["--new-session".to_owned()])
                .is_err(),
            "--new-session would replace the session bwrap builds for us"
        );
        let config = sandbox_config(dir.path())
            .with_extra_flags(vec!["--quiet".to_owned()])
            .expect("benign flag");
        let plan = plan(&config, &spec(&["true"]), dir.path()).expect("plan");
        assert_eq!(plan.argv[1], "--quiet", "extras first");
        assert_eq!(plan.argv[2], "--die-with-parent", "policy flags after");
    }

    #[test]
    fn a_read_only_root_uses_ro_bind_and_keeps_the_exceptions() {
        let dir = tempfile::tempdir().expect("tempdir");
        std::fs::create_dir(dir.path().join("out")).expect("out");
        let out = real(&dir.path().join("out"));
        let config = sandbox_config(dir.path()).with_writable(vec![out.clone()]);
        let mut request = spec(&["true"]);
        request.opts.write = WriteMode::ReadOnly;
        let plan = plan(&config, &request, &real(dir.path())).expect("plan");
        let joined = plan.argv.join(" ");
        let root = real(dir.path());
        assert!(
            joined.contains(&format!("--ro-bind {} {}", root.display(), root.display())),
            "{joined}"
        );
        assert!(
            joined.contains(&format!("--bind {} {}", out.display(), out.display())),
            "explicit writable paths survive a read-only call: {joined}"
        );
    }

    #[test]
    fn deny_all_isolates_the_network_and_hides_the_host_run() {
        let dir = tempfile::tempdir().expect("tempdir");
        let mut request = spec(&["true"]);
        request.opts.net.mode = NetMode::DenyAll;
        let plan = plan(&sandbox_config(dir.path()), &request, dir.path()).expect("plan");
        let joined = plan.argv.join(" ");
        assert!(joined.contains("--unshare-net"), "{joined}");
        // The private /run must come after the filesystem binds.
        let run = plan
            .argv
            .iter()
            .position(|arg| arg == "/run")
            .expect("/run");
        let root_bind = plan
            .argv
            .iter()
            .position(|arg| arg == "--ro-bind")
            .expect("ro-bind");
        assert!(run > root_bind, "/run must be mounted after the root bind");
    }

    #[test]
    fn the_proxy_modes_are_not_claimed_yet() {
        let dir = tempfile::tempdir().expect("tempdir");
        let mut request = spec(&["true"]);
        request.opts.net.mode = NetMode::AllowList;
        let error = plan(&sandbox_config(dir.path()), &request, dir.path()).expect_err("no proxy");
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::NotAvailable);
    }

    #[test]
    fn the_banned_table_covers_every_documented_tab() {
        // One entry per class, so a table edit cannot silently drop a class.
        for flag in [
            "--bind",
            "--ro-bind",
            "--setenv",
            "--unshare-net",
            "--chdir",
            "--seccomp",
            "--cap-add",
            "--args",
        ] {
            assert!(
                BANNED.contains(&flag),
                "{flag} must be rejected: extra flags are not a policy channel"
            );
            assert!(validate_extra_flags(&[flag.to_owned()]).is_err());
        }
        validate_extra_flags(&["--quiet".to_owned(), "--json-status-fd=3".to_owned()])
            .expect("benign flags pass");
    }

    #[test]
    fn a_flag_with_a_value_is_matched_by_its_name() {
        assert!(validate_extra_flags(&["--bind=/tmp/x".to_owned()]).is_err());
    }

    #[test]
    fn the_writable_list_mirrors_the_policy() {
        let dir = tempfile::tempdir().expect("tempdir");
        let config = sandbox_config(dir.path());
        assert_eq!(writable_binds(&config), vec![real(dir.path())]);
        let readonly = sandbox_config(dir.path()).with_readonly_root();
        assert!(writable_binds(&readonly).is_empty());
    }

    #[test]
    fn the_default_environment_and_options_are_untouched() {
        let dir = tempfile::tempdir().expect("tempdir");
        let request = spec(&["sh", "-c", "true"]);
        let plan = plan(&sandbox_config(dir.path()), &request, dir.path()).expect("plan");
        assert!(plan.env.is_empty(), "the wrapper needs no extra env yet");
        assert_eq!(plan.cwd, None, "bwrap chdirs itself");
    }

    #[test]
    fn a_spec_with_options_is_still_a_bwrap_invocation() {
        let dir = tempfile::tempdir().expect("tempdir");
        let mut request = spec(&["sh"]);
        request.opts = ExecOptions::default();
        let plan = plan(&sandbox_config(dir.path()), &request, dir.path()).expect("plan");
        assert_eq!(plan.argv[0], "bwrap");
    }
}
