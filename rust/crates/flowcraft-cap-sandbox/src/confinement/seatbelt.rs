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
use crate::net::proxy::{self, Endpoint};

/// Network section of a restricted profile, shared by allow-list and proxy mode.
///
/// macOS needs its platform sockets for TLS and network configuration; a blanket
/// `(deny network*)` also blocks `SecurityServer` and `configd`, and HTTPS then
/// fails at the handshake. This sequence is the one that actually works, so it is
/// reproduced rather than re-derived.
const PLATFORM_SOCKETS: &[&str] = &[
    "com.apple.bsd.dirhelper",
    "com.apple.system.opendirectoryd.membership",
    "com.apple.SecurityServer",
    "com.apple.networkd",
    "com.apple.ocspd",
    "com.apple.trustd.agent",
    "com.apple.SystemConfiguration.DNSConfiguration",
    "com.apple.SystemConfiguration.configd",
];

/// Build the argv that runs `spec` under the profile.
///
/// When the policy needs the proxy, `endpoint` is its loopback port: the profile
/// then allows exactly that one destination, and the proxy environment is
/// injected here — the runner knows the port, unlike the bwrap case where only the
/// in-namespace bridge does.
pub(crate) fn plan(
    config: &SandboxConfig,
    spec: &SessionSpec,
    workdir: &Path,
    endpoint: Option<&Endpoint>,
) -> Result<Plan> {
    let port = match (spec.opts.net.mode.uses_proxy(), endpoint) {
        (false, _) => None,
        (true, Some(Endpoint::Loopback(port))) => Some(*port),
        (true, Some(Endpoint::Unix(_))) => {
            return Err(Error::internal(
                "sandbox/seatbelt: the proxy must listen on loopback, not a unix socket",
            ));
        }
        (true, None) => {
            return Err(Error::not_available(format!(
                "sandbox/seatbelt: net mode {} needs the enforcement proxy",
                spec.opts.net.mode.as_str()
            )));
        }
    };
    let mut argv = vec![
        config.binary().to_owned(),
        "-p".to_owned(),
        profile(config, &spec.opts, port),
        "--".to_owned(),
    ];
    argv.extend(spec.argv.iter().cloned());
    let env = port.map_or_else(Vec::new, proxy::environment);
    Ok(Plan {
        argv,
        // sandbox-exec inherits the working directory; the inner runner sets it.
        cwd: Some(workdir.to_path_buf()),
        env,
    })
}

/// Render the SBPL profile for one call.
#[must_use]
pub(crate) fn profile(
    config: &SandboxConfig,
    opts: &ExecOptions,
    proxy_port: Option<u16>,
) -> String {
    let mut profile = String::from("(version 1)\n(allow default)\n(deny file-write*)\n");
    for path in writable_paths(config, opts.write) {
        profile.push_str(&format!(
            "(allow file-write* (subpath {}))\n",
            sbpl_string(&path.display().to_string())
        ));
    }
    // A shell that redirects to /dev/null must keep working.
    profile.push_str("(allow file-write* (literal \"/dev/null\"))\n");
    match (opts.net.mode, proxy_port) {
        (NetMode::DenyAll, _) => profile.push_str("(deny network*)\n"),
        (NetMode::AllowList | NetMode::Proxy, Some(port)) => {
            profile.push_str("(allow system-socket\n  (require-all\n    (socket-domain AF_SYSTEM)\n    (socket-protocol 2)\n  )\n)\n");
            profile.push_str("(allow mach-lookup\n");
            for name in PLATFORM_SOCKETS {
                profile.push_str(&format!("  (global-name {})\n", sbpl_string(name)));
            }
            profile.push_str(")\n");
            profile.push_str("(allow sysctl-read (sysctl-name-regex #\"^net.routetable\"))\n");
            profile.push_str("(deny network*)\n");
            profile.push_str(&format!(
                "(allow network-outbound (remote ip \"localhost:{port}\"))\n"
            ));
        }
        (NetMode::AllowList | NetMode::Proxy, None) => {
            // The caller must not reach here: `plan` refuses it. Denying is the
            // safe rendering if it ever does.
            profile.push_str("(deny network*)\n");
        }
        (NetMode::Default, _) => {}
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
        let profile = profile(&sandbox_config(dir.path()), &ExecOptions::default(), None);
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
        let profile = profile(&config, &opts, None);
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
        let profile = profile(&config, &ExecOptions::default(), None);
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
        assert!(profile(&sandbox_config(dir.path()), &opts, None).contains("(deny network*)"));
    }

    #[test]
    fn the_profile_is_the_third_argument() {
        let dir = tempfile::tempdir().expect("tempdir");
        let plan = plan(
            &sandbox_config(dir.path()),
            &spec(&["true"]),
            dir.path(),
            None,
        )
        .expect("plan");
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
    fn a_proxy_mode_without_an_endpoint_is_refused() {
        let dir = tempfile::tempdir().expect("tempdir");
        let mut request = spec(&["true"]);
        request.opts.net.mode = NetMode::Proxy;
        let error =
            plan(&sandbox_config(dir.path()), &request, dir.path(), None).expect_err("no proxy");
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::NotAvailable);
    }

    #[test]
    fn a_proxy_mode_opens_exactly_one_loopback_port() {
        let dir = tempfile::tempdir().expect("tempdir");
        let mut request = spec(&["true"]);
        request.opts.net.mode = NetMode::AllowList;
        let plan = plan(
            &sandbox_config(dir.path()),
            &request,
            dir.path(),
            Some(&Endpoint::Loopback(4321)),
        )
        .expect("plan");
        let rendered = &plan.argv[2];
        assert!(rendered.contains("(deny network*)"), "{rendered}");
        assert!(
            rendered.contains("(allow network-outbound (remote ip \"localhost:4321\"))"),
            "the single hole must be the proxy port: {rendered}"
        );
        assert!(
            rendered.contains("AF_SYSTEM"),
            "the platform sockets macOS needs for TLS must stay open: {rendered}"
        );
        // The environment is injected by the runner: it knows the port.
        let env: std::collections::BTreeMap<_, _> = plan.env.iter().cloned().collect();
        assert_eq!(
            env.get("HTTP_PROXY").map(String::as_str),
            Some("http://127.0.0.1:4321")
        );
        assert_eq!(env.get("NO_PROXY").map(String::as_str), Some(""));
    }

    #[test]
    fn a_unix_proxy_endpoint_is_a_programming_error_for_seatbelt() {
        let dir = tempfile::tempdir().expect("tempdir");
        let mut request = spec(&["true"]);
        request.opts.net.mode = NetMode::AllowList;
        let error = plan(
            &sandbox_config(dir.path()),
            &request,
            dir.path(),
            Some(&Endpoint::Unix(dir.path().join("p.sock"))),
        )
        .expect_err("seatbelt shares the host network stack");
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::Internal);
    }
}
