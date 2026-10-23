//! The sandbox contract.

use std::collections::BTreeMap;
use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use flowcraft_error::Result;
use flowcraft_resource::Resource;

/// The registry kind.
pub const KIND: &str = "sandbox.Runner";
/// The cross-process contract identity.
pub const CONTRACT: &str = "sandbox.Runner/v1";

/// What a runner can enforce.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Capabilities {
    /// Policy surface.
    pub policy: Enforcement,
    /// Session features.
    pub features: SessionFeatures,
}

/// Policy enforcement a runner guarantees.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Enforcement {
    /// The host environment is filtered through an allow-list.
    pub env_allow_list: bool,
    /// Network modes this backend can enforce (`default` is "no opinion").
    pub net_modes: Vec<String>,
    /// Filesystem write bounds.
    pub filesystem_bounds: bool,
    /// Write modes this backend can enforce.
    pub write_modes: Vec<String>,
    /// Memory caps.
    pub memory_cap: bool,
    /// CPU caps.
    pub cpu_cap: bool,
    /// Disk caps.
    pub disk_cap: bool,
}

/// The runner-level feature declaration (an alias kept for parity with Go's
/// `Capabilities.Features`).
pub type Features = SessionFeatures;

/// Session features a runner can provide.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct SessionFeatures {
    /// A TTY (PTY) is available.
    pub tty: bool,
    /// The session accepts stdin.
    pub stdin: bool,
    /// Named signals can be delivered.
    pub signal: bool,
    /// Structured events can be watched.
    pub events: bool,
}

/// How the child's environment is built.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct EnvPolicy {
    /// Host variables to pass through, by name.
    pub allow: Vec<String>,
    /// Variables to set explicitly.
    pub inject: BTreeMap<String, String>,
}

/// Network policy requested for the session.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct NetPolicy {
    /// `default` (no opinion) or a mode a backend can enforce.
    pub mode: String,
}

/// Filesystem write policy requested for the session.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub enum WriteMode {
    /// The backend's construction-time boundary.
    #[default]
    Default,
    /// Keep the root read-only for this call.
    ReadOnly,
}

/// Resource limits requested for the session.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ResourceLimits {
    /// CPU limit.
    pub cpu_millicores: Option<u32>,
    /// Memory limit in bytes.
    pub memory_bytes: Option<u64>,
    /// Disk limit in bytes.
    pub disk_bytes: Option<u64>,
    /// How much output the session buffers before dropping.
    pub max_output_bytes: Option<u64>,
}

/// What to run.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SessionSpec {
    /// Session id; the caller normally lets the runner assign one.
    pub id: String,
    /// `argv[0]` plus arguments.
    pub argv: Vec<String>,
    /// Whether the caller wants a TTY.
    pub tty: bool,
    /// Whether the caller will stream stdin.
    pub stdin: bool,
    /// Execution options.
    pub opts: ExecOptions,
}

/// Per-start options.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ExecOptions {
    /// Working directory.
    pub workdir: Option<String>,
    /// Bytes to deliver on stdin before returning from `start`.
    pub stdin: Option<Vec<u8>>,
    /// Kill the process group after this long.
    pub timeout: Option<Duration>,
    /// Environment policy.
    pub env: EnvPolicy,
    /// Network policy.
    pub net: NetPolicy,
    /// Write policy.
    pub write: WriteMode,
    /// Resource limits.
    pub limits: ResourceLimits,
}

/// How a session ended.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ExitStatus {
    /// Exit code, when the process exited normally.
    pub code: Option<i32>,
    /// Terminating signal, when it was killed.
    pub signal: Option<i32>,
    /// `exited`, `signal`, `timeout`, `terminated`, `output-limit`.
    pub reason: String,
}

/// One buffered output chunk.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Chunk {
    /// Monotonic sequence number, starting at 1.
    pub seq: u64,
    /// `stdout` or `stderr`.
    pub stream: String,
    /// Payload.
    pub data: Vec<u8>,
}

/// One pull read.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ReadResult {
    /// Sequence to pass as `after_seq` next time.
    pub next_seq: u64,
    /// Chunks after the requested sequence.
    pub chunks: Vec<Chunk>,
    /// Whether the session has ended and no chunks remain.
    pub eof: bool,
}

/// A started session.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SessionInfo {
    /// Session id.
    pub id: String,
    /// Command line.
    pub argv: Vec<String>,
    /// Process id, when known.
    pub pid: Option<u32>,
    /// When the session started, nanoseconds since the epoch.
    pub started_at_unix_nano: u64,
    /// What this session declared.
    pub features: SessionFeatures,
    /// Whether the process is still running.
    pub running: bool,
    /// Exit status, once finished.
    pub exit: Option<ExitStatus>,
}

/// A live session handle.
#[async_trait]
pub trait Session: Send + Sync + std::fmt::Debug {
    /// The session id.
    fn id(&self) -> &str;

    /// Process id, when known.
    fn pid(&self) -> Option<u32>;

    /// What this session actually provides.
    fn features(&self) -> SessionFeatures;

    /// Pull buffered output after `after_seq`.
    async fn read(&self, after_seq: u64, max_bytes: usize) -> Result<ReadResult>;

    /// Write to stdin.
    async fn write(&self, data: &[u8]) -> Result<()>;

    /// Close stdin.
    async fn close_input(&self) -> Result<()>;

    /// Wait for the process to end.
    async fn wait(&self) -> Result<ExitStatus>;

    /// Kill the session's process group.
    async fn terminate(&self) -> Result<()>;
}

/// A runner: process sessions under a policy.
#[async_trait]
pub trait Runner: Send + Sync + std::fmt::Debug {
    /// What this runner can enforce.
    ///
    /// Async because a remote runner has to ask its server. It must not fail
    /// into a default: an empty capability set would silently downgrade the
    /// caller's expectations.
    fn capabilities(&self) -> flowcraft_resource::BoxFuture<'_, Result<Capabilities>>;

    /// Start a session.
    async fn start(&self, spec: SessionSpec) -> Result<Arc<dyn Session>>;

    /// Look up a live session.
    async fn session(&self, id: &str) -> Result<Arc<dyn Session>>;

    /// Every known session.
    async fn list(&self) -> Result<Vec<SessionInfo>>;

    /// Terminate one session by id.
    async fn terminate(&self, id: &str) -> Result<()>;

    /// Terminate every session and release the runner.
    async fn close(&self) -> Result<()>;
}

/// The value a factory of this capability produces.
#[derive(Clone)]
pub struct Handle(pub Arc<dyn Runner>);

impl Handle {
    /// Wrap a runner.
    #[must_use]
    pub fn new(runner: Arc<dyn Runner>) -> Self {
        Self(runner)
    }

    /// The runner.
    #[must_use]
    pub fn runner(&self) -> Arc<dyn Runner> {
        self.0.clone()
    }
}

impl std::fmt::Debug for Handle {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("sandbox.Handle")
    }
}

impl std::ops::Deref for Handle {
    type Target = Arc<dyn Runner>;

    fn deref(&self) -> &Self::Target {
        &self.0
    }
}

impl Resource for Handle {
    fn shutdown(self: Arc<Self>) -> flowcraft_resource::BoxFuture<'static, Result<()>> {
        Box::pin(async move { self.0.close().await })
    }
}
