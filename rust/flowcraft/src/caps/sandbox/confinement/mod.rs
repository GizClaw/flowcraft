//! Confined runners: the same process contract, wrapped in an OS sandbox.
//!
//! `bwrap` (Linux) and `sandbox-exec` (macOS) are *wrappers*: the argv is
//! rewritten to run the child under the sandbox, and the inner local runner
//! keeps doing the parts it is good at (env allow-list, timeout, process-group
//! kill, bounded output, cursors). Only what the wrapper actually enforces is
//! promoted into capabilities.
//!
//! Two properties make this module reviewable:
//!
//! - the wrapper's argv and profile are built by **pure functions** ([`bwrap`],
//!   [`seatbelt`]) returning a [`Plan`], so the exact flags can be asserted
//!   without spawning anything;
//! - the runner root is resolved once, at build time ([`path`]), and every call
//!   is checked against it twice — lexically and through symlinks.

mod bwrap;
pub(crate) mod path;
mod seatbelt;

use std::path::{Path, PathBuf};
use std::sync::Arc;

use crate::error::{Error, Result};
use crate::resource::Kind;
use crate::resource::{BoxFuture, BuildCtx, Factory, Phase, Spec};
use async_trait::async_trait;
use serde::Deserialize;
use serde_json::Value;

use crate::caps::sandbox::codes::{self, Code};
use crate::caps::sandbox::journal::{Config as JournalConfig, Engine as Journal};
use crate::caps::sandbox::net::proxy;

use crate::caps::sandbox::contract::{
    self, CONTRACT, Capabilities, Enforcement, Features, Handle, JournalCapabilities, KIND,
    NetModes, Runner, Session, SessionInfo, SessionSpec, WriteMode, WriteModes,
};
use crate::caps::sandbox::local::LocalRunner;

/// Where a wrapper puts the extra mount tree when it needs one.
pub(crate) const RUN_DIR: &str = "/run";

/// One spawn plan: what to execute, where, and with which extra environment.
///
/// A pure value, so a flag list or a profile can be snapshot-tested directly.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct Plan {
    /// Full argv, starting with the wrapper binary.
    pub argv: Vec<String>,
    /// Working directory for the wrapper process.
    pub cwd: Option<PathBuf>,
    /// Extra environment for the wrapper itself.
    pub env: Vec<(String, String)>,
}

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

    /// The default binary name.
    #[must_use]
    pub const fn default_binary(self) -> &'static str {
        match self {
            Self::Bwrap => "bwrap",
            Self::Seatbelt => "sandbox-exec",
        }
    }

    /// The binary to run, honouring a host override.
    #[must_use]
    pub fn binary(self, override_path: Option<&str>) -> String {
        match override_path {
            Some(path) if !path.trim().is_empty() => path.to_owned(),
            _ => self.default_binary().to_owned(),
        }
    }

    /// What this wrapper genuinely enforces.
    ///
    /// Both backends confine writes to the configured root, so both may claim a
    /// filesystem boundary. Memory/CPU caps come from the inner runner (they are
    /// enforced by the same supervisor), and neither backend enforces a network
    /// mode that needs the proxy or a tty in this build.
    #[must_use]
    pub fn capabilities(self) -> Capabilities {
        Capabilities {
            policy: Enforcement {
                env_allow_list: true,
                net: NetModes {
                    deny_all: true,
                    allow_list: false,
                    proxy: false,
                },
                socks5: false,
                mitm: false,
                unix_sockets: self == Self::Bwrap,
                memory_cap: false,
                cpu_cap: false,
                disk_cap: false,
                filesystem_bounds: true,
                write: WriteModes {
                    workspace: true,
                    read_only: true,
                },
            },
            features: Features {
                // The pty is inherited by the wrapper, so confinement keeps
                // whatever the platform can provide.
                tty: crate::caps::sandbox::session::terminal::available(),
                // Per session, from the spec.
                stdin: false,
                signal: cfg!(unix),
                events: true,
            },
            journal: JournalCapabilities::default(),
        }
    }

    /// Build the plan for one session.
    pub(crate) fn plan(
        self,
        config: &SandboxConfig,
        spec: &SessionSpec,
        workdir: &Path,
        endpoint: Option<&crate::caps::sandbox::net::proxy::Endpoint>,
    ) -> Result<Plan> {
        match self {
            Self::Bwrap => bwrap::plan(config, spec, workdir, endpoint),
            Self::Seatbelt => seatbelt::plan(config, spec, workdir, endpoint),
        }
    }
}

/// The network wiring of one confined runner.
#[derive(Debug, Clone, Default)]
pub struct NetSettings {
    /// The binary re-executed as the in-namespace bridge (bwrap only).
    ///
    /// A deployment points this at `flowcraftd`, whose `main` calls
    /// [`crate::caps::sandbox::net::bridge::run_if_invoked`] first; without it, bwrap cannot
    /// offer `allow_list` / `proxy` and says so instead of degrading to deny-all.
    pub bridge: Option<PathBuf>,
    /// Send one audit record per decision to `tracing`.
    pub audit: bool,
}

/// The resolved configuration of one confined runner.
#[derive(Clone)]
pub struct SandboxConfig {
    sandbox: Sandbox,
    root: PathBuf,
    writable: Vec<PathBuf>,
    readonly_root: bool,
    binary: String,
    extra_flags: Vec<String>,
    journal: Option<Arc<Journal>>,
    net: NetSettings,
}

impl std::fmt::Debug for SandboxConfig {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("sandbox.SandboxConfig")
            .field("sandbox", &self.sandbox)
            .field("root", &self.root)
            .field("writable", &self.writable)
            .field("readonly_root", &self.readonly_root)
            .finish_non_exhaustive()
    }
}

impl SandboxConfig {
    /// A configuration rooted at `root` with no exceptions.
    ///
    /// The root is canonicalized when it exists, so a caller that passes a
    /// symlinked path (`/var/...` on macOS) still gets containment checks
    /// against the same spelling the resolver produces.
    #[must_use]
    pub fn new(sandbox: Sandbox, root: impl Into<PathBuf>) -> Self {
        let root = root.into();
        let root = std::fs::canonicalize(&root).unwrap_or(root);
        Self {
            sandbox,
            root,
            writable: Vec::new(),
            readonly_root: false,
            binary: sandbox.default_binary().to_owned(),
            extra_flags: Vec::new(),
            journal: None,
            net: NetSettings::default(),
        }
    }

    /// Override the wrapper binary.
    #[must_use]
    pub fn with_binary(mut self, binary: Option<&str>) -> Self {
        self.binary = self.sandbox.binary(binary);
        self
    }

    /// Additional writable paths.
    ///
    /// Canonicalized like the root: an SBPL `subpath` rule is matched against the
    /// resolved vnode path, so a symlinked spelling (`/var/...` on macOS) would
    /// silently grant nothing.
    #[must_use]
    pub fn with_writable(mut self, paths: Vec<PathBuf>) -> Self {
        self.writable = paths
            .into_iter()
            .map(|path| std::fs::canonicalize(&path).unwrap_or(path))
            .collect();
        self
    }

    /// Pin the root read-only for every call.
    #[must_use]
    pub fn with_readonly_root(mut self) -> Self {
        self.readonly_root = true;
        self
    }

    /// Extra wrapper flags, validated against the backend's rejection table.
    pub fn with_extra_flags(mut self, flags: Vec<String>) -> Result<Self> {
        match self.sandbox {
            Sandbox::Bwrap => bwrap::validate_extra_flags(&flags)?,
            // `sandbox-exec` has no flag channel: every argument is a profile.
            Sandbox::Seatbelt => {
                if !flags.is_empty() {
                    return Err(Error::validation(
                        "sandbox/seatbelt: extra_flags is only valid for bwrap",
                    ));
                }
            }
        }
        self.extra_flags = flags;
        Ok(self)
    }

    /// The backend.
    #[must_use]
    pub fn sandbox(&self) -> Sandbox {
        self.sandbox
    }

    pub(crate) fn root(&self) -> &Path {
        &self.root
    }

    pub(crate) fn writable(&self) -> &[PathBuf] {
        &self.writable
    }

    pub(crate) fn readonly_root(&self) -> bool {
        self.readonly_root
    }

    pub(crate) fn binary(&self) -> &str {
        &self.binary
    }

    pub(crate) fn extra_flags(&self) -> &[String] {
        &self.extra_flags
    }

    /// Attach a file journal.
    #[must_use]
    pub(crate) fn with_journal(mut self, journal: Arc<Journal>) -> Self {
        self.journal = Some(journal);
        self
    }

    /// Configure the network wiring.
    #[must_use]
    pub fn with_net(mut self, net: NetSettings) -> Self {
        self.net = net;
        self
    }

    pub(crate) fn net(&self) -> &NetSettings {
        &self.net
    }

    /// Whether this backend can enforce `allow_list` / `proxy` as configured.
    ///
    /// seatbelt only needs the proxy (it shares the host network stack); bwrap
    /// also needs a bridge binary to reach the socket from inside its namespace.
    #[must_use]
    pub fn proxy_capable(&self) -> bool {
        match self.sandbox {
            Sandbox::Seatbelt => true,
            Sandbox::Bwrap => self.net.bridge.is_some(),
        }
    }
}

/// A runner that wraps commands in an OS sandbox.
pub struct ConfinedRunner {
    inner: LocalRunner,
    config: SandboxConfig,
}

impl std::fmt::Debug for ConfinedRunner {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("sandbox.ConfinedRunner")
            .field("config", &self.config)
            .finish_non_exhaustive()
    }
}

impl ConfinedRunner {
    /// Build a runner from an already-resolved configuration.
    #[must_use]
    pub fn new(config: SandboxConfig) -> Self {
        Self {
            // The wrapper resolves the workdir, so the inner runner must not
            // re-check it against a root it does not have.
            inner: LocalRunner::new(),
            config,
        }
    }

    /// Rewrite a spec so the inner runner executes the sandbox wrapper, starting
    /// the enforcement proxy when the policy needs one.
    async fn prepare(&self, mut spec: SessionSpec) -> Result<Prepared> {
        if spec.argv.is_empty() {
            return Err(codes::validation(Code::ArgvEmpty, "sandbox: argv is empty"));
        }
        spec.opts.net.validate()?;
        let workdir = path::resolve_workdir(&self.config.root, spec.opts.workdir.as_deref())?;

        let proxied = spec.opts.net.mode.uses_proxy();
        if proxied {
            if !self.config.proxy_capable() {
                return Err(codes::unavailable(
                    Code::NetModeUnsupported,
                    format!(
                        "sandbox/{}: net mode {} needs settings.net.bridge",
                        self.config.sandbox.impl_name(),
                        spec.opts.net.mode.as_str()
                    ),
                ));
            }
            // The proxy owns these variables: letting a caller set them would be
            // a way to point the child at a destination the rules never saw.
            for name in proxy::OWNED_ENV {
                if spec
                    .opts
                    .env
                    .inject
                    .keys()
                    .any(|key| key.eq_ignore_ascii_case(name))
                {
                    return Err(Error::validation(format!(
                        "sandbox: {name} is owned by the sandbox while a network policy is enforced"
                    )));
                }
            }
        }

        let listen = match self.config.sandbox {
            // seatbelt shares the host network stack, so a loopback port is the
            // one hole its profile can allow.
            Sandbox::Seatbelt => proxy::Listen::Loopback,
            // bwrap gets a unix socket bind-mounted into its namespace.
            Sandbox::Bwrap => proxy::Listen::Unix(PathBuf::from(format!(
                "/tmp/flowcraft-proxy-{}.sock",
                if spec.id.is_empty() {
                    format!("{}", std::process::id())
                } else {
                    spec.id.clone()
                }
            ))),
        };
        let audit: Option<proxy::Audit> = self.config.net().audit.then(|| {
            Arc::new(|decision: &proxy::Decision| {
                tracing::info!(
                    host = %decision.host,
                    port = decision.port,
                    allow = decision.allow,
                    mode = decision.mode.as_str(),
                    rule = decision.rule.as_deref().unwrap_or(""),
                    reason = decision.reason.as_deref().unwrap_or(""),
                    "sandbox: network decision"
                );
            }) as proxy::Audit
        });
        let proxy = if proxied {
            Some(
                proxy::start(proxy::Config {
                    policy: spec.opts.net.clone(),
                    listen,
                    audit,
                })
                .await?,
            )
        } else {
            None
        };

        let plan = self.config.sandbox.plan(
            &self.config,
            &spec,
            &workdir,
            proxy.as_ref().map(|proxy| proxy.endpoint()),
        );
        let plan = match plan {
            Ok(plan) => plan,
            Err(error) => {
                if let Some(proxy) = proxy {
                    proxy.shutdown().await;
                }
                return Err(error);
            }
        };

        spec.argv = plan.argv;
        // The wrapper consumed these; the inner runner would reject them and
        // they must not be applied twice.
        spec.opts.net.mode = contract::NetMode::Default;
        spec.opts.write = WriteMode::Workspace;
        spec.opts.workdir = plan.cwd;
        for (name, value) in plan.env {
            spec.opts.env.inject.insert(name, value);
        }
        Ok(Prepared { spec, proxy })
    }
}

/// A spec that is ready to hand to the inner runner, plus the proxy it needs to
/// keep alive.
struct Prepared {
    spec: SessionSpec,
    proxy: Option<proxy::Proxy>,
}

#[async_trait]
impl Runner for ConfinedRunner {
    fn capabilities(&self) -> crate::resource::BoxFuture<'_, Result<Capabilities>> {
        Box::pin(async move {
            let mut capabilities = self.config.sandbox.capabilities();
            // Resource caps are enforced by the inner runner's supervisor, so the
            // claim comes from there rather than from the wrapper.
            let inner = self.inner.capabilities().await?;
            capabilities.policy.memory_cap = inner.policy.memory_cap;
            capabilities.policy.cpu_cap = inner.policy.cpu_cap;
            // `allow_list` / `proxy` are only claimed when the wiring for this
            // backend exists: seatbelt always, bwrap only with a bridge binary.
            let proxied = self.config.proxy_capable();
            capabilities.policy.net.allow_list = proxied;
            capabilities.policy.net.proxy = proxied;
            // `socks5` is "the proxy can dial a socks5 *upstream*" (the Go
            // meaning). This crate only terminates SOCKS5 for the child, so the
            // field stays false until upstream chaining exists.
            capabilities.journal = self
                .config
                .journal
                .as_ref()
                .map_or_else(JournalCapabilities::default, |journal| {
                    journal.capabilities()
                });
            Ok(capabilities)
        })
    }

    async fn start(&self, spec: SessionSpec) -> Result<Arc<dyn Session>> {
        let prepared = self.prepare(spec).await?;
        let session = match self.inner.start(prepared.spec).await {
            Ok(session) => session,
            Err(error) => {
                if let Some(proxy) = prepared.proxy {
                    proxy.shutdown().await;
                }
                return Err(error);
            }
        };
        if let Some(proxy) = prepared.proxy {
            // The proxy lives exactly as long as the session it enforces for: the
            // socket is bound before the spawn and removed once the child is gone.
            let watched = session.clone();
            tokio::spawn(async move {
                let _ = watched.wait().await;
                proxy.shutdown().await;
            });
        }
        Ok(session)
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

    async fn close_session(&self, id: &str) -> Result<()> {
        self.inner.close_session(id).await
    }

    async fn close(&self) -> Result<()> {
        self.inner.close().await
    }

    async fn open_journal(&self) -> Result<Arc<dyn crate::caps::sandbox::journal::Journal>> {
        self.config.journal.as_ref().map_or_else(
            || {
                Err(Error::not_available(format!(
                    "sandbox/{}: no file journal is attached to this runner",
                    self.config.sandbox.impl_name()
                )))
            },
            |journal| Ok(journal.clone() as Arc<dyn crate::caps::sandbox::journal::Journal>),
        )
    }
}

#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
struct Settings {
    /// The filesystem boundary, relative to the process working directory.
    root: String,
    /// Override the sandbox binary.
    #[serde(default)]
    binary: Option<String>,
    /// Extra writable paths: relative paths are joined to the root.
    #[serde(default)]
    writable_paths: Vec<String>,
    /// Keep the root read-only for every call.
    #[serde(default)]
    readonly_root: bool,
    /// Extra wrapper flags (`bwrap` only).
    #[serde(default)]
    extra_flags: Vec<String>,
    /// Attach a file journal (see `journal::Config`).
    #[serde(default)]
    journal: Option<Value>,
    /// Network wiring: the bridge binary and the audit switch.
    #[serde(default)]
    net: NetInput,
}

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(deny_unknown_fields)]
struct NetInput {
    /// The binary re-executed as the in-namespace bridge (bwrap only).
    #[serde(default)]
    bridge: Option<PathBuf>,
    /// Send one audit record per decision to `tracing`.
    #[serde(default)]
    audit: bool,
}

fn parse(value: &Value) -> Result<Settings> {
    crate::resource::settings(value)
}

fn check_common(settings: &Settings) -> Result<()> {
    if settings.root.trim().is_empty() {
        return Err(Error::validation("sandbox: settings.root is required"));
    }
    if let Some(binary) = &settings.binary
        && binary.trim().is_empty()
    {
        return Err(Error::validation(
            "sandbox: settings.binary must not be empty",
        ));
    }
    for (index, path) in settings.writable_paths.iter().enumerate() {
        if path.trim().is_empty() {
            return Err(Error::validation(format!(
                "sandbox: settings.writable_paths[{index}] is empty"
            )));
        }
    }
    Ok(())
}

/// Schema validation for `bwrap`. Resolution (which touches the filesystem)
/// happens in `build`.
fn validate_bwrap(value: &Value) -> Result<()> {
    let settings = parse(value)?;
    check_common(&settings)?;
    bwrap::validate_extra_flags(&settings.extra_flags)?;
    check_journal(&settings)?;
    check_net(&settings)
}

/// Schema validation for `seatbelt`.
fn validate_seatbelt(value: &Value) -> Result<()> {
    let settings = parse(value)?;
    check_common(&settings)?;
    if !settings.extra_flags.is_empty() {
        return Err(Error::validation(
            "sandbox/seatbelt: extra_flags is only valid for bwrap",
        ));
    }
    check_journal(&settings)
}

fn check_net(settings: &Settings) -> Result<()> {
    if let Some(bridge) = &settings.net.bridge
        && bridge.as_os_str().is_empty()
    {
        return Err(Error::validation("sandbox: net.bridge must not be empty"));
    }
    Ok(())
}

fn check_journal(settings: &Settings) -> Result<()> {
    if let Some(journal) = &settings.journal {
        JournalConfig::parse(journal)?;
    }
    Ok(())
}

fn factory(sandbox: Sandbox, validate: fn(&Value) -> Result<()>) -> Spec {
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
        let root = path::resolve_root(&settings.root)?;
        let writable =
            path::resolve_writable(&root, &settings.writable_paths, settings.readonly_root)?;
        let mut config = SandboxConfig {
            sandbox,
            root: root.clone(),
            writable,
            readonly_root: settings.readonly_root,
            binary: sandbox.binary(settings.binary.as_deref()),
            extra_flags: settings.extra_flags,
            journal: None,
            net: NetSettings {
                bridge: settings.net.bridge.clone(),
                audit: settings.net.audit,
            },
        };
        if let Some(value) = &settings.journal {
            // A deployment that asked for a journal must not end up with a
            // runner that silently watches nothing.
            let journal_config = JournalConfig::parse(value)?;
            config = config.with_journal(Journal::open(&root, &journal_config)?);
        }
        let runner = ConfinedRunner::new(config);
        Ok(Arc::new(Handle::new(Arc::new(runner))))
    })
}

/// The `sandbox.Runner/bwrap` factory.
pub struct BwrapFactory;

impl Factory for BwrapFactory {
    type Output = Handle;

    fn spec(&self) -> Spec {
        factory(Sandbox::Bwrap, validate_bwrap)
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
        factory(Sandbox::Seatbelt, validate_seatbelt)
    }

    fn build<'a>(&'a self, ctx: BuildCtx<'a>) -> BoxFuture<'a, Result<Arc<Self::Output>>> {
        build(Sandbox::Seatbelt, ctx)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn seatbelt_available() -> bool {
        std::path::Path::new("/usr/bin/sandbox-exec").exists()
    }

    fn config(root: &Path) -> SandboxConfig {
        SandboxConfig::new(Sandbox::Seatbelt, root)
    }

    fn spec(argv: &[&str]) -> SessionSpec {
        SessionSpec::new(argv.iter().map(|arg| (*arg).to_owned()).collect())
    }

    #[test]
    fn both_backends_claim_a_filesystem_boundary() {
        for sandbox in [Sandbox::Bwrap, Sandbox::Seatbelt] {
            let capabilities = sandbox.capabilities();
            assert!(capabilities.policy.filesystem_bounds, "{sandbox:?}");
            assert!(capabilities.policy.write.workspace, "{sandbox:?}");
            assert!(capabilities.policy.write.read_only, "{sandbox:?}");
            assert!(capabilities.policy.net.deny_all, "{sandbox:?}");
            assert!(!capabilities.policy.net.allow_list, "{sandbox:?}");
            assert!(!capabilities.policy.memory_cap, "{sandbox:?}");
            // A pty is inherited by the wrapper, so confinement keeps whatever
            // the platform can provide. Asserting `true` pins a mechanism; this
            // asserts the inheritance, which is the actual claim — and it is what
            // a host without a pty backend (Windows, today) reports.
            assert_eq!(
                capabilities.features.tty,
                crate::caps::sandbox::session::terminal::available(),
                "{sandbox:?}"
            );
        }
        assert!(Sandbox::Bwrap.capabilities().policy.unix_sockets);
        assert!(!Sandbox::Seatbelt.capabilities().policy.unix_sockets);
    }

    #[test]
    fn the_proxy_claim_follows_the_wiring() {
        let dir = tempfile::tempdir().expect("tempdir");
        // seatbelt only needs the proxy itself.
        assert!(SandboxConfig::new(Sandbox::Seatbelt, dir.path()).proxy_capable());
        // bwrap also needs something inside its namespace to reach the socket.
        assert!(!SandboxConfig::new(Sandbox::Bwrap, dir.path()).proxy_capable());
        let with_bridge = SandboxConfig::new(Sandbox::Bwrap, dir.path()).with_net(NetSettings {
            bridge: Some(dir.path().join("flowcraftd")),
            audit: false,
        });
        assert!(with_bridge.proxy_capable());
    }

    #[test]
    fn extra_flags_are_backend_specific() {
        let dir = tempfile::tempdir().expect("tempdir");
        assert!(
            config(dir.path())
                .with_extra_flags(vec!["--quiet".to_owned()])
                .is_err(),
            "seatbelt has no flag channel"
        );
        SandboxConfig::new(Sandbox::Bwrap, dir.path())
            .with_extra_flags(vec!["--quiet".to_owned()])
            .expect("bwrap accepts benign flags");
    }

    #[tokio::test]
    async fn a_workdir_outside_the_root_is_refused_before_spawning() {
        let dir = tempfile::tempdir().expect("tempdir");
        let root_dir = dir.path().join("root");
        std::fs::create_dir(&root_dir).expect("root");
        let runner = ConfinedRunner::new(config(&root_dir));
        let mut request = spec(&["true"]);
        request.opts.workdir = Some(PathBuf::from(".."));
        let error = runner.start(request).await.expect_err("traversal");
        assert_eq!(error.kind(), crate::error::ErrorKind::Forbidden);
        assert!(error.to_string().contains("path traversal"), "{error}");
    }

    #[tokio::test]
    async fn the_seatbelt_wrapper_runs_a_command() {
        if !seatbelt_available() {
            return;
        }
        let dir = tempfile::tempdir().expect("tempdir");
        let runner = ConfinedRunner::new(config(dir.path()));
        let session = runner
            .start(spec(&["sh", "-c", "echo confined"]))
            .await
            .expect("start");
        let exit = session.wait().await.expect("wait");
        assert_eq!(exit.code, Some(0));
        let mut out = String::new();
        let mut after = 0;
        loop {
            let read = session.read(after, 64 * 1024).await.expect("read");
            for chunk in &read.chunks {
                out.push_str(&String::from_utf8_lossy(&chunk.data));
            }
            after = read.next_seq;
            if read.eof {
                break;
            }
        }
        assert!(out.contains("confined"), "{out}");
        runner.close().await.expect("close");
    }

    #[tokio::test]
    async fn the_seatbelt_profile_confines_writes_to_the_root() {
        if !seatbelt_available() {
            return;
        }
        let dir = tempfile::tempdir().expect("tempdir");
        let root_dir = dir.path().join("root");
        std::fs::create_dir(&root_dir).expect("root");
        let inside = root_dir.join("inside.txt");
        let outside = dir.path().join("outside.txt");
        let runner = ConfinedRunner::new(config(&root_dir));

        // Inside the root: allowed.
        let session = runner
            .start(spec(&[
                "sh",
                "-c",
                &format!("echo ok > {}", inside.display()),
            ]))
            .await
            .expect("start");
        let exit = session.wait().await.expect("wait");
        assert_eq!(exit.code, Some(0), "a write inside the root must succeed");
        assert!(inside.exists(), "the file must exist on the host");

        // Outside the root: denied by the profile, not merely by convention.
        let session = runner
            .start(spec(&[
                "sh",
                "-c",
                &format!("echo no > {}", outside.display()),
            ]))
            .await
            .expect("start");
        let exit = session.wait().await.expect("wait");
        assert_ne!(exit.code, Some(0), "a write outside the root must fail");
        assert!(!outside.exists(), "the file must not have been created");

        runner.close().await.expect("close");
    }

    #[tokio::test]
    async fn the_proxy_is_the_only_way_out() {
        if !seatbelt_available() || !std::path::Path::new("/usr/bin/curl").exists() {
            return;
        }
        let dir = tempfile::tempdir().expect("tempdir");
        let root_dir = dir.path().join("root");
        std::fs::create_dir(&root_dir).expect("root");
        let runner = ConfinedRunner::new(config(&root_dir));
        // No rule allows anything, so the proxy must refuse and the reason must
        // reach the child: that proves the environment reached it and the socket
        // hole is the only egress.
        let mut request = spec(&["curl", "-s", "--max-time", "5", "http://example.com/"]);
        request.opts.net.mode = contract::NetMode::AllowList;
        let session = runner.start(request).await.expect("start");
        session.wait().await.expect("wait");
        let mut out = String::new();
        let mut after = 0;
        loop {
            let read = session.read(after, 64 * 1024).await.expect("read");
            for chunk in &read.chunks {
                out.push_str(&String::from_utf8_lossy(&chunk.data));
            }
            after = read.next_seq;
            if read.eof {
                break;
            }
        }
        assert!(
            out.contains("not reachable under this policy"),
            "the child must have reached the proxy and been refused: {out:?}"
        );
        runner.close().await.expect("close");
    }

    #[tokio::test]
    async fn a_caller_cannot_take_over_the_proxy_variables() {
        if !seatbelt_available() {
            return;
        }
        let dir = tempfile::tempdir().expect("tempdir");
        let root_dir = dir.path().join("root");
        std::fs::create_dir(&root_dir).expect("root");
        let runner = ConfinedRunner::new(config(&root_dir));
        let mut request = spec(&["true"]);
        request.opts.net.mode = contract::NetMode::AllowList;
        request
            .opts
            .env
            .inject
            .insert("http_proxy".to_owned(), "http://127.0.0.1:1".to_owned());
        let error = runner.start(request).await.expect_err("owned variable");
        assert_eq!(error.kind(), crate::error::ErrorKind::Validation);
        assert!(
            error.to_string().contains("owned by the sandbox"),
            "{error}"
        );
    }

    #[tokio::test]
    async fn a_pinned_read_only_root_denies_writes_inside_it() {
        if !seatbelt_available() {
            return;
        }
        let dir = tempfile::tempdir().expect("tempdir");
        let root_dir = dir.path().join("root");
        std::fs::create_dir(&root_dir).expect("root");
        let inside = root_dir.join("blocked.txt");
        let runner = ConfinedRunner::new(config(&root_dir).with_readonly_root());
        let session = runner
            .start(spec(&[
                "sh",
                "-c",
                &format!("echo no > {}", inside.display()),
            ]))
            .await
            .expect("start");
        let exit = session.wait().await.expect("wait");
        assert_ne!(exit.code, Some(0));
        assert!(!inside.exists());
        runner.close().await.expect("close");
    }

    #[tokio::test]
    async fn an_explicit_writable_path_survives_a_read_only_call() {
        if !seatbelt_available() {
            return;
        }
        let dir = tempfile::tempdir().expect("tempdir");
        let root_dir = dir.path().join("root");
        let out_dir = dir.path().join("out");
        std::fs::create_dir(&root_dir).expect("root");
        std::fs::create_dir(&out_dir).expect("out");
        let target = out_dir.join("allowed.txt");
        let runner = ConfinedRunner::new(config(&root_dir).with_writable(vec![out_dir.clone()]));
        let mut request = spec(&["sh", "-c", &format!("echo ok > {}", target.display())]);
        request.opts.write = WriteMode::ReadOnly;
        let session = runner.start(request).await.expect("start");
        let exit = session.wait().await.expect("wait");
        assert_eq!(exit.code, Some(0), "the granted exception stays writable");
        assert!(target.exists());
        runner.close().await.expect("close");
    }
}
