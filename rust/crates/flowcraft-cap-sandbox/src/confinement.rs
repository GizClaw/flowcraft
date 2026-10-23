//! Confined runners: the same process contract, wrapped in an OS sandbox.
//!
//! `bwrap` (Linux) and `sandbox-exec` (macOS) are *wrappers*: the argv is
//! rewritten to run the child under the sandbox, and the inner local runner
//! keeps doing the parts it is good at (env allow-list, timeout, process-group
//! kill, bounded output). Only the parts a wrapper can actually enforce are
//! promoted into capabilities.

use std::sync::Arc;

use async_trait::async_trait;
use flowcraft_error::{Error, Result};
use flowcraft_resource::Kind;
use flowcraft_resource::{BoxFuture, BuildCtx, Factory, Phase, Spec};
use serde::Deserialize;
use serde_json::Value;

use crate::contract::{
    self, CONTRACT, Capabilities, Enforcement, ExecOptions, Features, Handle, KIND, Runner,
    Session, SessionInfo, SessionSpec, WriteMode,
};
use crate::local::LocalRunner;

/// Which sandbox binary wraps the command.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Sandbox {
    /// `bwrap` on Linux.
    Bwrap,
    /// `sandbox-exec` on macOS.
    Seatbelt,
}

impl Sandbox {
    /// The implementation name in the registry.
    #[must_use]
    pub const fn impl_name(self) -> &'static str {
        match self {
            Self::Bwrap => "bwrap",
            Self::Seatbelt => "seatbelt",
        }
    }

    /// The binary to run, honouring a host override.
    #[must_use]
    pub fn binary(self, override_path: Option<&str>) -> String {
        match override_path {
            Some(path) if !path.trim().is_empty() => path.to_owned(),
            _ => match self {
                Self::Bwrap => "bwrap".to_owned(),
                Self::Seatbelt => "sandbox-exec".to_owned(),
            },
        }
    }

    /// What this wrapper can enforce.
    #[must_use]
    pub fn capabilities(self) -> Capabilities {
        Capabilities {
            policy: Enforcement {
                env_allow_list: true,
                net_modes: vec!["default".to_owned(), "deny-all".to_owned()],
                filesystem_bounds: true,
                write_modes: vec!["default".to_owned(), "read_only".to_owned()],
                memory_cap: false,
                cpu_cap: false,
                disk_cap: false,
            },
            features: Features {
                tty: false,
                stdin: true,
                signal: false,
                events: false,
            },
        }
    }

    /// Build the wrapper argv for one session.
    #[must_use]
    pub fn argv(self, binary: &str, spec: &SessionSpec) -> Vec<String> {
        let mut argv = vec![binary.to_owned()];
        match self {
            Self::Bwrap => {
                argv.extend(
                    [
                        "--die-with-parent",
                        "--ro-bind",
                        "/",
                        "/",
                        "--dev",
                        "/dev",
                        "--proc",
                        "/proc",
                        "--tmpfs",
                        "/tmp",
                    ]
                    .into_iter()
                    .map(str::to_owned),
                );
                if spec.opts.net.mode == "deny-all" {
                    argv.push("--unshare-net".to_owned());
                }
                if spec.opts.write != WriteMode::ReadOnly
                    && let Some(workdir) = &spec.opts.workdir
                {
                    argv.push("--bind".to_owned());
                    argv.push(workdir.clone());
                    argv.push(workdir.clone());
                }
                argv.push("--chdir".to_owned());
                argv.push(spec.opts.workdir.clone().unwrap_or_else(|| "/".to_owned()));
                argv.push("--".to_owned());
                argv.extend(spec.argv.iter().cloned());
            }
            Self::Seatbelt => {
                argv.push("-p".to_owned());
                argv.push(seatbelt_profile(&spec.opts));
                argv.push("--".to_owned());
                argv.extend(spec.argv.iter().cloned());
            }
        }
        argv
    }
}

/// The seatbelt profile for one session.
///
/// `sandbox-exec` is deprecated upstream but still the only per-process
/// confinement macOS ships without entitlements.
#[must_use]
pub fn seatbelt_profile(opts: &ExecOptions) -> String {
    let mut profile = String::from("(version 1)\n(allow default)\n");
    if opts.net.mode == "deny-all" {
        profile.push_str("(deny network*)\n");
    }
    if opts.write == WriteMode::ReadOnly {
        profile.push_str("(deny file-write*)\n");
    }
    profile
}

/// A runner that wraps commands in an OS sandbox.
pub struct ConfinedRunner {
    inner: LocalRunner,
    sandbox: Sandbox,
    binary: String,
}

impl std::fmt::Debug for ConfinedRunner {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ConfinedRunner")
            .field("sandbox", &self.sandbox)
            .field("binary", &self.binary)
            .finish_non_exhaustive()
    }
}

impl ConfinedRunner {
    /// Build a runner for `sandbox`.
    #[must_use]
    pub fn new(sandbox: Sandbox, binary: Option<&str>) -> Self {
        Self {
            inner: LocalRunner::new(),
            sandbox,
            binary: sandbox.binary(binary),
        }
    }

    /// Rewrite a spec so the inner runner executes the sandbox wrapper.
    fn wrap(&self, mut spec: SessionSpec) -> Result<SessionSpec> {
        if spec.argv.is_empty() {
            return Err(Error::validation("sandbox: argv is empty"));
        }
        if spec.tty {
            return Err(Error::not_available(
                "sandbox: confined runners do not provide a TTY",
            ));
        }
        match spec.opts.net.mode.as_str() {
            "" | "default" | "deny-all" => {}
            other => {
                return Err(Error::not_available(format!(
                    "sandbox: net mode {other:?} is not supported"
                )));
            }
        }
        let argv = self.sandbox.argv(&self.binary, &spec);
        spec.argv = argv;
        // The wrapper consumed these; the inner runner would reject them.
        spec.opts.net.mode = "default".to_owned();
        spec.opts.write = WriteMode::Default;
        // bwrap chdirs itself, seatbelt inherits the inner runner's cwd.
        if self.sandbox == Sandbox::Bwrap {
            spec.opts.workdir = None;
        }
        Ok(spec)
    }
}

#[async_trait]
impl Runner for ConfinedRunner {
    fn capabilities(&self) -> flowcraft_resource::BoxFuture<'_, Result<Capabilities>> {
        Box::pin(async move { Ok(self.sandbox.capabilities()) })
    }

    async fn start(&self, spec: SessionSpec) -> Result<Arc<dyn Session>> {
        let wrapped = self.wrap(spec)?;
        self.inner.start(wrapped).await
    }

    async fn session(&self, id: &str) -> Result<Arc<dyn Session>> {
        self.inner.session(id).await
    }

    async fn list(&self) -> Result<Vec<SessionInfo>> {
        self.inner.list().await
    }

    async fn terminate(&self, id: &str) -> Result<()> {
        self.inner.terminate(id).await
    }

    async fn close(&self) -> Result<()> {
        self.inner.close().await
    }
}

#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
struct Settings {
    /// Override the sandbox binary path.
    #[serde(default)]
    binary: Option<String>,
}

fn validate(value: &Value) -> Result<()> {
    let settings: Settings = flowcraft_resource::settings(value)?;
    if let Some(binary) = &settings.binary
        && binary.trim().is_empty()
    {
        return Err(Error::validation("settings.binary must not be empty"));
    }
    Ok(())
}

fn factory(sandbox: Sandbox) -> Spec {
    let kind = Kind::new(KIND).unwrap_or_else(|error| panic!("{KIND} is invalid: {error}"));
    Spec {
        contract: Some(CONTRACT),
        imp: Some(sandbox.impl_name()),
        deps: Vec::new(),
        phase: Phase::Main,
        validate_settings: Some(validate),
        ..Spec::new(kind)
    }
}

fn build<'a>(sandbox: Sandbox, ctx: BuildCtx<'a>) -> BoxFuture<'a, Result<Arc<Handle>>> {
    Box::pin(async move {
        let settings: Settings = ctx.settings()?;
        let runner = ConfinedRunner::new(sandbox, settings.binary.as_deref());
        Ok(Arc::new(Handle::new(Arc::new(runner))))
    })
}

/// The `sandbox.Runner/bwrap` factory.
pub struct BwrapFactory;

impl Factory for BwrapFactory {
    type Output = Handle;

    fn spec(&self) -> Spec {
        factory(Sandbox::Bwrap)
    }

    fn build<'a>(&'a self, ctx: BuildCtx<'a>) -> BoxFuture<'a, Result<Arc<Self::Output>>> {
        build(Sandbox::Bwrap, ctx)
    }
}

/// The `sandbox.Runner/seatbelt` factory.
pub struct SeatbeltFactory;

impl Factory for SeatbeltFactory {
    type Output = Handle;

    fn spec(&self) -> Spec {
        factory(Sandbox::Seatbelt)
    }

    fn build<'a>(&'a self, ctx: BuildCtx<'a>) -> BoxFuture<'a, Result<Arc<Self::Output>>> {
        build(Sandbox::Seatbelt, ctx)
    }
}

/// The `contract` import is used by the builders above; keep it referenced.
const _: &str = contract::CONTRACT;

#[cfg(test)]
mod tests {
    use super::*;

    fn spec(argv: &[&str]) -> SessionSpec {
        SessionSpec {
            id: String::new(),
            argv: argv.iter().map(|arg| (*arg).to_owned()).collect(),
            tty: false,
            stdin: false,
            opts: ExecOptions::default(),
        }
    }

    #[test]
    fn bwrap_locks_down_the_root_and_swaps_in_the_command() {
        let mut request = spec(&["sh", "-c", "true"]);
        request.opts.net.mode = "default".to_owned();
        let argv = Sandbox::Bwrap.argv("bwrap", &request);
        assert_eq!(argv[0], "bwrap");
        assert!(argv.contains(&"--die-with-parent".to_owned()));
        assert!(argv.contains(&"--ro-bind".to_owned()));
        assert!(!argv.contains(&"--unshare-net".to_owned()));
        let tail = argv.split(|arg| arg == "--").nth(1).expect("command tail");
        assert_eq!(tail, ["sh", "-c", "true"]);
    }

    #[test]
    fn bwrap_denies_network_and_binds_the_workdir_when_writable() {
        let mut request = spec(&["true"]);
        request.opts.net.mode = "deny-all".to_owned();
        request.opts.workdir = Some("/work".to_owned());
        let argv = Sandbox::Bwrap.argv("bwrap", &request);
        assert!(argv.contains(&"--unshare-net".to_owned()));
        let index = argv.iter().position(|arg| arg == "--bind").expect("bind");
        assert_eq!(&argv[index + 1..index + 3], ["/work", "/work"]);
    }

    #[test]
    fn bwrap_read_only_omits_the_bind() {
        let mut request = spec(&["true"]);
        request.opts.write = WriteMode::ReadOnly;
        request.opts.workdir = Some("/work".to_owned());
        let argv = Sandbox::Bwrap.argv("bwrap", &request);
        assert!(!argv.contains(&"--bind".to_owned()));
    }

    #[test]
    fn seatbelt_profiles_encode_the_policy() {
        let mut request = spec(&["true"]);
        assert_eq!(
            seatbelt_profile(&request.opts),
            "(version 1)\n(allow default)\n"
        );
        request.opts.net.mode = "deny-all".to_owned();
        request.opts.write = WriteMode::ReadOnly;
        let profile = seatbelt_profile(&request.opts);
        assert!(profile.contains("(deny network*)"), "{profile}");
        assert!(profile.contains("(deny file-write*)"), "{profile}");
    }

    #[test]
    fn capabilities_are_what_the_wrapper_can_enforce() {
        let capabilities = Sandbox::Seatbelt.capabilities();
        assert!(capabilities.policy.filesystem_bounds);
        assert!(
            capabilities
                .policy
                .net_modes
                .contains(&"deny-all".to_owned())
        );
        assert!(!capabilities.policy.memory_cap);
        assert!(!capabilities.features.tty);
    }

    #[tokio::test]
    async fn unsupported_net_modes_are_refused() {
        let runner = ConfinedRunner::new(Sandbox::Seatbelt, None);
        let mut request = spec(&["true"]);
        request.opts.net.mode = "allow-list".to_owned();
        let error = runner.start(request).await.expect_err("unsupported");
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::NotAvailable);
    }

    #[tokio::test]
    async fn the_seatbelt_wrapper_runs_a_command() {
        if !std::path::Path::new("/usr/bin/sandbox-exec").exists() {
            return;
        }
        let runner = ConfinedRunner::new(Sandbox::Seatbelt, None);
        let session = runner
            .start(spec(&["sh", "-c", "echo confined"]))
            .await
            .expect("start");
        let exit = session.wait().await.expect("wait");
        assert_eq!(exit.code, Some(0));
        let mut out = String::new();
        let mut after = 0;
        for _ in 0..8 {
            let read = session.read(after, 64 * 1024).await.expect("read");
            for chunk in &read.chunks {
                out.push_str(&String::from_utf8_lossy(&chunk.data));
            }
            after = read.next_seq;
            if read.chunks.is_empty() {
                break;
            }
        }
        assert!(out.contains("confined"), "{out}");
    }

    #[tokio::test]
    async fn the_seatbelt_read_only_profile_denies_writes() {
        if !std::path::Path::new("/usr/bin/sandbox-exec").exists() {
            return;
        }
        let runner = ConfinedRunner::new(Sandbox::Seatbelt, None);
        let mut request = spec(&["sh", "-c", "echo x > /tmp/flowcraft-seatbelt-denied"]);
        request.opts.write = WriteMode::ReadOnly;
        let session = runner.start(request).await.expect("start");
        let exit = session.wait().await.expect("wait");
        assert_ne!(exit.code, Some(0), "write should have been denied");
    }
}
