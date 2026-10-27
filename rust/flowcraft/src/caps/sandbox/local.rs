//! The local runner: processes on this host, no OS confinement.
//!
//! What it does enforce: an environment allow-list, a working directory, a
//! timeout that kills the process group, and a bounded replayable output
//! buffer. What it does **not** enforce: filesystem confinement, network
//! policy, memory/CPU/disk caps, or a pty. Those are reported honestly by
//! [`Runner::capabilities`], and a spec that asks for one is rejected with
//! `NotAvailable` — never silently downgraded.
//!
//! The session machinery itself lives in [`crate::caps::sandbox::session`]; this type is the
//! contract implementation plus its factory.

use std::path::PathBuf;
use std::sync::Arc;

use crate::error::{Error, Result};
use crate::resource::Kind;
use crate::resource::{BoxFuture, BuildCtx, Factory, Phase, Spec};
use async_trait::async_trait;
use serde::Deserialize;
use serde_json::Value;

use crate::caps::sandbox::contract::{
    CONTRACT, Capabilities, Enforcement, Features, Handle, JournalCapabilities, KIND, NetModes,
    Runner, Session, SessionInfo, SessionSpec, WriteModes,
};
use crate::caps::sandbox::journal::{Config as JournalConfig, Engine as Journal};
use crate::caps::sandbox::session::{DEFAULT_OUTPUT_LIMIT, Registry, Spawn, spawn};

/// A runner that spawns processes on this host.
pub struct LocalRunner {
    registry: Registry,
    /// The filesystem boundary used to validate a working directory. There is
    /// no OS enforcement behind it (see [`Runner::capabilities`]), but it makes
    /// a `workdir` escape a refusal rather than a coincidence.
    root: Option<PathBuf>,
    default_output_limit: u64,
    journal: Option<Arc<Journal>>,
    limits: Arc<crate::caps::sandbox::limits::Manager>,
}

impl std::fmt::Debug for LocalRunner {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("sandbox.LocalRunner")
    }
}

impl Default for LocalRunner {
    fn default() -> Self {
        Self::new()
    }
}

impl LocalRunner {
    /// A runner without a root: every `workdir` is checked for existence only.
    ///
    /// This is what a confined backend uses for its inner execution, because the
    /// wrapper already resolved the path against a real boundary.
    #[must_use]
    pub fn new() -> Self {
        Self {
            registry: Registry::new(),
            root: None,
            default_output_limit: DEFAULT_OUTPUT_LIMIT,
            journal: None,
            // Probed once: creating and removing a control group is not something
            // to do per session, and the answer is a property of the host.
            limits: Arc::new(crate::caps::sandbox::limits::Manager::probe()),
        }
    }

    /// A runner whose working directories must stay inside `root`.
    ///
    /// The root is canonicalized when it exists (see [`SandboxConfig::new`]),
    /// so containment is compared between real paths.
    #[must_use]
    // Assembly: there is no executor to block while a capability is being built.
    #[allow(clippy::disallowed_methods)]
    pub fn at(root: impl Into<PathBuf>) -> Self {
        let root = root.into();
        Self {
            root: Some(std::fs::canonicalize(&root).unwrap_or(root)),
            ..Self::new()
        }
    }

    /// The root this runner validates against, if any.
    #[must_use]
    pub fn root(&self) -> Option<&std::path::Path> {
        self.root.as_deref()
    }

    /// Attach a file journal.
    #[must_use]
    pub(crate) fn with_journal(mut self, journal: Arc<Journal>) -> Self {
        self.journal = Some(journal);
        self
    }

    /// The output cap for sessions that do not declare one.
    #[must_use]
    pub fn with_output_limit(mut self, bytes: u64) -> Self {
        self.default_output_limit = bytes.max(1);
        self
    }

    /// The spawn function the registry calls.
    fn spawner(&self) -> Spawn {
        let options = Arc::new(crate::caps::sandbox::session::SpawnOptions {
            root: self.root.clone(),
            default_output_limit: self.default_output_limit,
            limits: self.limits.clone(),
        });
        Arc::new(move |spec: SessionSpec| {
            let options = options.clone();
            Box::pin(async move { spawn(spec, &options).await })
        })
    }
}

#[async_trait]
impl Runner for LocalRunner {
    fn capabilities(&self) -> BoxFuture<'_, Result<Capabilities>> {
        Box::pin(async move {
            let limits = self.limits.support();
            Ok(Capabilities {
                policy: Enforcement {
                    env_allow_list: true,
                    // `default` is the absence of a policy, not an enforceable
                    // posture, so no net mode is claimed.
                    net: NetModes::default(),
                    socks5: false,
                    mitm: false,
                    unix_sockets: false,
                    memory_cap: limits.memory,
                    cpu_cap: limits.cpu,
                    // No local backend has a quota mechanism.
                    disk_cap: false,
                    // The workdir check is call-time validation only: once the
                    // child runs it can chdir anywhere, so this is not a
                    // filesystem boundary.
                    filesystem_bounds: false,
                    write: WriteModes {
                        // A local runner's writable area is the whole filesystem,
                        // so there is no "workspace" to narrow to.
                        workspace: false,
                        // Windows honours a read-only call with a Low-integrity
                        // token, which the kernel enforces as no-write-up; unix has
                        // no mechanism here, so it keeps refusing.
                        read_only: cfg!(windows),
                    },
                },
                features: Features {
                    tty: crate::caps::sandbox::session::terminal::available(),
                    // `stdin` is decided per session (by the spec), not by the
                    // backend, so the runner-level answer is `false`.
                    stdin: false,
                    signal: cfg!(unix),
                    events: true,
                },
                journal: self
                    .journal
                    .as_ref()
                    .map_or_else(JournalCapabilities::default, |journal| {
                        journal.capabilities()
                    }),
            })
        })
    }

    async fn start(&self, spec: SessionSpec) -> Result<Arc<dyn Session>> {
        self.registry.start(spec, &self.spawner()).await
    }

    async fn session(&self, id: &str) -> Result<Arc<dyn Session>> {
        self.registry.get(id)
    }

    async fn list(&self) -> Result<Vec<SessionInfo>> {
        self.registry.list()
    }

    async fn terminate(&self, id: &str) -> Result<()> {
        self.registry.terminate(id).await
    }

    async fn close_session(&self, id: &str) -> Result<()> {
        self.registry.close_session(id).await
    }

    async fn close(&self) -> Result<()> {
        self.registry.close().await
    }

    async fn open_journal(&self) -> Result<Arc<dyn crate::caps::sandbox::journal::Journal>> {
        self.journal.as_ref().map_or_else(
            || {
                Err(Error::not_available(
                    "sandbox/local: no file journal is attached to this runner",
                ))
            },
            |journal| Ok(journal.clone() as Arc<dyn crate::caps::sandbox::journal::Journal>),
        )
    }
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct Settings {
    /// The filesystem boundary the runner validates working directories against.
    root: String,
    #[serde(default)]
    max_output_bytes: Option<u64>,
    /// Attach a file journal. The subtree being present is the opt-in: an absent
    /// key costs nothing, and a typo fails the build.
    #[serde(default)]
    journal: Option<Value>,
}

fn validate(value: &Value) -> Result<()> {
    let settings: Settings = crate::resource::settings(value)?;
    if settings.root.trim().is_empty() {
        return Err(Error::validation("sandbox: settings.root is required"));
    }
    if settings.max_output_bytes == Some(0) {
        return Err(Error::validation(
            "sandbox/local: max_output_bytes must be positive",
        ));
    }
    if let Some(journal) = &settings.journal {
        JournalConfig::parse(journal)?;
    }
    Ok(())
}

/// The `sandbox.Runner/local` factory.
pub struct LocalFactory;

impl Factory for LocalFactory {
    type Output = Handle;

    fn spec(&self) -> Spec {
        let kind = Kind::new(KIND).unwrap_or_else(|error| panic!("{KIND} is invalid: {error}"));
        Spec {
            contract: Some(CONTRACT),
            imp: Some("local"),
            deps: Vec::new(),
            phase: Phase::Main,
            validate_settings: Some(validate),
            ..Spec::new(kind)
        }
    }

    fn build<'a>(&'a self, ctx: BuildCtx<'a>) -> BoxFuture<'a, Result<Arc<Self::Output>>> {
        Box::pin(async move {
            let settings: Settings = ctx.settings()?;
            let root = crate::caps::sandbox::confinement::path::resolve_root(&settings.root)?;
            let mut runner = LocalRunner::at(root.clone());
            if let Some(limit) = settings.max_output_bytes {
                runner = runner.with_output_limit(limit);
            }
            if let Some(value) = &settings.journal {
                // A deployment that asked for a journal must not end up with a
                // runner that silently watches nothing.
                let config = JournalConfig::parse(value)?;
                let journal = Journal::open(&root, &config)?;
                runner = runner.with_journal(journal);
            }
            Ok(Arc::new(Handle::new(Arc::new(runner))))
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::caps::sandbox::contract::ExitReason;

    fn spec(argv: &[&str]) -> SessionSpec {
        SessionSpec::new(argv.iter().map(|arg| (*arg).to_owned()).collect())
    }

    async fn read_all(session: &Arc<dyn Session>) -> String {
        let mut out = String::new();
        let mut after = 0;
        loop {
            let read = session.read(after, 64 * 1024).await.unwrap();
            for chunk in &read.chunks {
                out.push_str(&String::from_utf8_lossy(&chunk.data));
            }
            after = read.next_seq;
            if read.eof {
                return out;
            }
        }
    }

    #[tokio::test]
    async fn runs_a_command_through_the_contract() {
        let runner = LocalRunner::new();
        let session = runner.start(spec(&["sh", "-c", "echo hi"])).await.unwrap();
        assert_eq!(session.wait().await.unwrap().reason, ExitReason::Exited);
        assert!(read_all(&session).await.contains("hi"));
        runner.close().await.unwrap();
    }

    #[tokio::test]
    async fn the_registry_is_the_only_session_table() {
        let runner = LocalRunner::new();
        let session = runner.start(spec(&["sh", "-c", "exit 0"])).await.unwrap();
        let id = session.id().to_owned();
        assert!(runner.session(&id).await.is_ok());
        assert_eq!(runner.list().await.unwrap().len(), 1);
        session.wait().await.unwrap();

        runner.close_session(&id).await.unwrap();
        assert!(runner.session(&id).await.is_err());
        assert!(runner.list().await.unwrap().is_empty());
    }

    #[tokio::test]
    async fn capabilities_are_honest() {
        let capabilities = LocalRunner::new().capabilities().await.unwrap();
        assert!(capabilities.policy.env_allow_list);
        assert!(!capabilities.policy.filesystem_bounds);
        assert_eq!(capabilities.policy.net, NetModes::default());
        assert_eq!(
            capabilities.policy.write,
            WriteModes {
                workspace: false,
                read_only: cfg!(windows),
            }
        );
        assert_eq!(
            capabilities.features.tty,
            crate::caps::sandbox::session::terminal::available()
        );
        assert!(capabilities.features.events);
        assert!(!capabilities.journal.enabled);
    }

    #[tokio::test]
    async fn closing_terminates_a_running_session() {
        let runner = LocalRunner::new();
        let session = runner.start(spec(&["sleep", "30"])).await.unwrap();
        runner.close().await.unwrap();
        let exit = session.wait().await.unwrap();
        assert_eq!(exit.reason, ExitReason::Terminated);
    }
}

#[cfg(test)]
mod root_tests {
    use super::*;

    fn spec(argv: &[&str]) -> SessionSpec {
        SessionSpec::new(argv.iter().map(|arg| (*arg).to_owned()).collect())
    }

    #[tokio::test]
    async fn a_working_directory_inside_the_root_is_used() {
        let dir = tempfile::tempdir().expect("tempdir");
        std::fs::create_dir(dir.path().join("sub")).expect("sub");
        let runner = LocalRunner::at(dir.path());
        let mut request = spec(&["sh", "-c", "pwd"]);
        request.opts.workdir = Some(PathBuf::from("sub"));
        let session = runner.start(request).await.expect("start");
        session.wait().await.expect("wait");
        let mut out = String::new();
        let mut after = 0;
        loop {
            let read = session.read(after, 4096).await.expect("read");
            for chunk in &read.chunks {
                out.push_str(&String::from_utf8_lossy(&chunk.data));
            }
            after = read.next_seq;
            if read.eof {
                break;
            }
        }
        assert!(
            out.trim().ends_with("/sub"),
            "the child must run in the resolved workdir: {out:?}"
        );
        runner.close().await.expect("close");
    }

    #[tokio::test]
    async fn a_working_directory_outside_the_root_is_refused() {
        let dir = tempfile::tempdir().expect("tempdir");
        let runner = LocalRunner::at(dir.path());
        let mut request = spec(&["sh", "-c", "true"]);
        request.opts.workdir = Some(PathBuf::from(".."));
        let error = runner.start(request).await.expect_err("traversal");
        assert_eq!(error.kind(), crate::error::ErrorKind::Forbidden);
        assert!(error.to_string().contains("path traversal"), "{error}");
    }

    #[tokio::test]
    async fn without_a_root_only_existence_is_checked() {
        let runner = LocalRunner::new();
        let mut request = spec(&["sh", "-c", "true"]);
        request.opts.workdir = Some(PathBuf::from("/"));
        runner
            .start(request)
            .await
            .expect("a rootless runner accepts any existing directory");
        runner.close().await.expect("close");
    }
}
