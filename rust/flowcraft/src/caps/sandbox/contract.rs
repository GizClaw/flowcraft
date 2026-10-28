//! The sandbox contract.
//!
//! One [`Runner`] starts process sessions under a policy, and one [`Session`]
//! is the handle a caller drives. Everything a backend can differ on is either
//! declared up front ([`Capabilities`]) or answered per session
//! ([`SessionFeatures`]); nothing is discovered by trying an operation and
//! inspecting the error.
//!
//! The wire types are deliberately *not* the domain types: [`crate::caps::sandbox::wire`]
//! owns every `proto ⇄ domain` conversion, so an unknown spelling on the wire
//! is a `Validation` error at the boundary instead of a silently downgraded
//! policy inside the runner.

use std::collections::BTreeMap;
use std::path::PathBuf;
use std::pin::Pin;
use std::sync::Arc;
use std::time::Duration;

use crate::error::{Error, Result};
use crate::resource::Resource;
use async_trait::async_trait;
use tokio_stream::Stream as EventSource;

/// The registry kind.
pub const KIND: &str = "sandbox.Runner";
/// The cross-process contract identity.
pub const CONTRACT: &str = "sandbox.Runner/v1";

// ---------------------------------------------------------------------------
// Network policy
// ---------------------------------------------------------------------------

/// The network posture a session runs under.
///
/// This is the *mode* only; the parameters a mode needs live in [`NetPolicy`],
/// because a capability declaration answers "which modes" and a request
/// answers "which destinations".
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Hash)]
pub enum NetMode {
    /// No opinion: whatever the host does.
    #[default]
    Default,
    /// No outbound connection at all.
    DenyAll,
    /// Only destinations that a rule allows.
    AllowList,
    /// Everything goes through the configured upstream proxy, minus denies.
    Proxy,
}

impl NetMode {
    /// The stable spelling used on the wire and in settings.
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Default => "default",
            Self::DenyAll => "deny_all",
            Self::AllowList => "allow_list",
            Self::Proxy => "proxy",
        }
    }

    /// Decode a wire spelling. An unknown value is an error, never a fallback:
    /// silently reading `allow_list` as `default` would widen the policy.
    pub fn parse(raw: &str) -> Result<Self> {
        match raw {
            "" | "default" => Ok(Self::Default),
            "deny_all" | "deny-all" => Ok(Self::DenyAll),
            "allow_list" | "allow-list" => Ok(Self::AllowList),
            "proxy" => Ok(Self::Proxy),
            other => Err(Error::validation(format!(
                "sandbox: unknown net mode {other:?}"
            ))),
        }
    }

    /// Whether this mode needs a host-side enforcement proxy.
    #[must_use]
    pub const fn uses_proxy(self) -> bool {
        matches!(self, Self::AllowList | Self::Proxy)
    }
}

/// What one rule decides.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum NetAction {
    /// Refuse the destination.
    Deny,
    /// Permit the destination.
    Allow,
}

impl NetAction {
    /// The stable spelling used on the wire and in settings.
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Deny => "deny",
            Self::Allow => "allow",
        }
    }

    /// Decode a wire spelling.
    pub fn parse(raw: &str) -> Result<Self> {
        match raw {
            "deny" => Ok(Self::Deny),
            "allow" => Ok(Self::Allow),
            other => Err(Error::validation(format!(
                "sandbox: unknown net rule action {other:?}"
            ))),
        }
    }
}

/// One host/port-level allow or deny rule.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct NetRule {
    /// The verdict when this rule matches.
    pub action: NetAction,
    /// Hostname pattern, IP literal, or CIDR block.
    pub host: String,
    /// Destination port; `0` matches any port.
    pub port: u16,
    /// Model-facing explanation attached to a deny rule.
    pub reason: Option<String>,
}

/// The network policy requested for one session.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct NetPolicy {
    /// The posture.
    pub mode: NetMode,
    /// Legacy allow-list: hostname suffixes and exact IP literals.
    pub allow_hosts: Vec<String>,
    /// Explicit rules. Deny rules win across the whole set.
    pub rules: Vec<NetRule>,
    /// Upstream proxy URL for [`NetMode::Proxy`]: `http://` or `socks5://`.
    pub proxy: Option<String>,
    /// Unix sockets the child may reach.
    pub unix_sockets: Vec<PathBuf>,
}

impl NetPolicy {
    /// A policy that expresses no opinion.
    #[must_use]
    pub fn default_policy() -> Self {
        Self::default()
    }

    /// Check the cross-backend invariants of this policy.
    ///
    /// A backend that cannot enforce a field rejects the request later; this
    /// only rejects policies that are malformed for *every* backend.
    pub fn validate(&self) -> Result<()> {
        if let Some(proxy) = &self.proxy {
            let lower = proxy.to_ascii_lowercase();
            let rest = if lower.starts_with("http://") {
                &proxy["http://".len()..]
            } else if lower.starts_with("socks5://") {
                &proxy["socks5://".len()..]
            } else {
                return Err(Error::validation(format!(
                    "sandbox: proxy {proxy:?} must be http:// or socks5://"
                )));
            };
            let authority = rest.split('/').next().unwrap_or_default();
            let host = authority.rsplit('@').next().unwrap_or_default();
            if host.is_empty() {
                return Err(Error::validation(format!(
                    "sandbox: proxy {proxy:?} has no host"
                )));
            }
            if let Some((user, _)) = authority.rsplit_once('@')
                && user.is_empty()
                && authority.contains(':')
            {
                return Err(Error::validation(format!(
                    "sandbox: proxy {proxy:?} has a password but no username"
                )));
            }
            // `proxy` is honoured by both `allow_list` and `proxy` mode (Go
            // accepts both), so the field is validated on its own and the
            // backend decides whether it can chain.
        }
        for (index, rule) in self.rules.iter().enumerate() {
            if rule.host.trim().is_empty() {
                return Err(Error::validation(format!(
                    "sandbox: net rule {index} has an empty host"
                )));
            }
            if let Some(reason) = &rule.reason {
                if reason.trim() != reason {
                    return Err(Error::validation(format!(
                        "sandbox: net rule {index} reason has leading or trailing whitespace"
                    )));
                }
                if reason.contains('\r') || reason.contains('\n') {
                    return Err(Error::validation(format!(
                        "sandbox: net rule {index} reason must be a single line"
                    )));
                }
            }
        }
        for path in &self.unix_sockets {
            if !path.is_absolute() {
                return Err(Error::validation(format!(
                    "sandbox: unix socket {} must be an absolute path",
                    path.display()
                )));
            }
        }
        Ok(())
    }
}

// ---------------------------------------------------------------------------
// Filesystem, limits, environment
// ---------------------------------------------------------------------------

/// How much write access one call gets on top of the runner's own boundary.
///
/// There is deliberately no "wider" value: a call narrows the construction-time
/// boundary and never widens it.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub enum WriteMode {
    /// The runner's construction-time writable area, whatever that is.
    #[default]
    Workspace,
    /// Keep the runner's root read-only for this call.
    ReadOnly,
}

impl WriteMode {
    /// The stable spelling used on the wire and in settings.
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Workspace => "workspace",
            Self::ReadOnly => "read_only",
        }
    }

    /// Decode a wire spelling.
    pub fn parse(raw: &str) -> Result<Self> {
        match raw {
            "" | "default" | "workspace" => Ok(Self::Workspace),
            "read_only" | "read-only" => Ok(Self::ReadOnly),
            other => Err(Error::validation(format!(
                "sandbox: unknown write mode {other:?}"
            ))),
        }
    }
}

/// Resource caps requested for one session.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ResourceLimits {
    /// CPU budget in thousandths of a core.
    pub cpu_millicores: Option<u32>,
    /// Memory cap in bytes.
    pub memory_bytes: Option<u64>,
    /// Disk cap in bytes.
    pub disk_bytes: Option<u64>,
    /// How many bytes of output the session buffers before dropping the oldest.
    pub max_output_bytes: Option<u64>,
}

/// How the child's environment is built.
///
/// `allow: None` inherits the host environment (the historical behaviour);
/// `allow: Some([])` inherits nothing and leaves only [`inject`]; `allow:
/// Some([names])` forwards exactly those names. `inject` is applied last and
/// wins over an inherited value.
///
/// [`inject`]: EnvPolicy::inject
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct EnvPolicy {
    /// Host variable names to pass through.
    pub allow: Option<Vec<String>>,
    /// Variables to set explicitly.
    pub inject: BTreeMap<String, String>,
}

impl EnvPolicy {
    /// Narrow `allow` with the provider's policy.
    ///
    /// A provider may only tighten: a caller that asked for less keeps less, a
    /// caller that asked for nothing (or for everything, by leaving it unset) is
    /// bounded by what the deployment configured. Injection is untouched — adding
    /// a variable is not a narrowing, so it is not a provider's move here.
    ///
    /// The order of the caller's list is preserved, because the resulting list is
    /// what a human reads in a `Debug` print or an audit record.
    pub fn tighten(&mut self, provider: Option<&[String]>) {
        let Some(provider) = provider else {
            return;
        };
        self.allow = Some(match self.allow.take() {
            None => provider.to_vec(),
            Some(requested) => requested
                .into_iter()
                .filter(|name| provider.iter().any(|allowed| allowed == name))
                .collect(),
        });
    }

    /// Check a provider's list of names it will forward.
    ///
    /// A name that is empty or carries `=` would be silently ignored by the child's
    /// environment lookup, so a deployment that meant to forward a credential and
    /// mistyped it learns at build time instead of at the first failed call.
    pub fn validate_allow_names(names: &[String]) -> Result<()> {
        for (index, name) in names.iter().enumerate() {
            if name.is_empty() || name.contains('=') || name.trim() != name {
                return Err(Error::validation(format!(
                    "sandbox: env_allow[{index}] {name:?} is not a variable name"
                )));
            }
        }
        Ok(())
    }

    /// Whether the child inherits the daemon's whole environment.
    ///
    /// `true` is the contract's default and the one a deployment should think
    /// about: everything the daemon was started with — including whatever is in
    /// its environment — reaches the child.
    #[must_use]
    pub fn inherits_host(&self) -> bool {
        self.allow.is_none()
    }
}

/// Per-start options.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ExecOptions {
    /// Working directory, resolved against the runner's root.
    pub workdir: Option<PathBuf>,
    /// Bytes to deliver on stdin before returning from `start`.
    ///
    /// One-shot input belongs to [`crate::caps::sandbox::exec`]; `start` neither delivers
    /// these bytes nor lets them declare an input side.
    pub stdin: Option<Vec<u8>>,
    /// Kill the process group after this long.
    pub timeout: Option<Duration>,
    /// Environment policy.
    pub env: EnvPolicy,
    /// Network policy.
    pub net: NetPolicy,
    /// Write policy for this call.
    pub write: WriteMode,
    /// Resource caps.
    pub limits: ResourceLimits,
}

// ---------------------------------------------------------------------------
// Session spec / outcome
// ---------------------------------------------------------------------------

/// What to run.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SessionSpec {
    /// Session id; empty lets the runner assign one.
    pub id: String,
    /// `argv[0]` plus arguments.
    pub argv: Vec<String>,
    /// Whether the caller wants a pty.
    pub tty: bool,
    /// Initial pty height in rows (`0` means 24).
    pub rows: u16,
    /// Initial pty width in columns (`0` means 80).
    pub cols: u16,
    /// Whether the caller will stream stdin.
    pub stdin: bool,
    /// Execution options.
    pub opts: ExecOptions,
}

impl SessionSpec {
    /// A non-interactive spec for `argv`.
    #[must_use]
    pub fn new(argv: Vec<String>) -> Self {
        Self {
            id: String::new(),
            argv,
            tty: false,
            rows: 0,
            cols: 0,
            stdin: false,
            opts: ExecOptions::default(),
        }
    }

    /// The pty window size, defaulted.
    #[must_use]
    pub fn window(&self) -> (u16, u16) {
        (
            if self.rows == 0 { 24 } else { self.rows },
            if self.cols == 0 { 80 } else { self.cols },
        )
    }

    /// Whether the child gets an input side.
    #[must_use]
    pub fn wants_stdin(&self) -> bool {
        self.tty || self.stdin
    }
}

/// Which output stream one chunk belongs to.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum Stream {
    /// Standard output.
    Stdout,
    /// Standard error.
    Stderr,
    /// A merged pty stream.
    Tty,
}

impl Stream {
    /// The stable spelling used on the wire.
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Stdout => "stdout",
            Self::Stderr => "stderr",
            Self::Tty => "tty",
        }
    }

    /// Decode a wire spelling.
    pub fn parse(raw: &str) -> Result<Self> {
        match raw {
            "stdout" => Ok(Self::Stdout),
            "stderr" => Ok(Self::Stderr),
            "tty" => Ok(Self::Tty),
            other => Err(Error::validation(format!(
                "sandbox: unknown stream {other:?}"
            ))),
        }
    }
}

/// A soft signal a session can receive.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum Signal {
    /// Ctrl-C semantics: the process may catch it and keep running.
    Interrupt,
}

impl Signal {
    /// The stable spelling used on the wire.
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Interrupt => "interrupt",
        }
    }

    /// Decode a wire spelling.
    pub fn parse(raw: &str) -> Result<Self> {
        match raw {
            "interrupt" | "INT" | "SIGINT" => Ok(Self::Interrupt),
            other => Err(Error::validation(format!(
                "sandbox: unknown signal {other:?}"
            ))),
        }
    }
}

/// Why a process ended.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Default)]
pub enum ExitReason {
    /// Normal exit, including a non-zero code.
    #[default]
    Exited,
    /// The OS reported death by a signal the session did not send.
    Signaled,
    /// [`Session::terminate`] stopped it.
    Terminated,
    /// The per-call timeout elapsed.
    TimedOut,
    /// A resource cap killed it.
    BudgetExceeded,
    /// A cap could not be enforced, so the session was killed rather than run
    /// unguarded.
    Unenforceable,
}

impl ExitReason {
    /// The stable spelling used on the wire.
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Exited => "exited",
            Self::Signaled => "signaled",
            Self::Terminated => "terminated",
            Self::TimedOut => "timed_out",
            Self::BudgetExceeded => "budget_exceeded",
            Self::Unenforceable => "unenforceable",
        }
    }

    /// Decode a wire spelling.
    ///
    /// The older Rust spelling (`signal`, `timeout`) is accepted on input so a
    /// peer built before this rename still interoperates.
    pub fn parse(raw: &str) -> Result<Self> {
        match raw {
            "" | "exited" => Ok(Self::Exited),
            "signal" | "signaled" => Ok(Self::Signaled),
            "terminated" => Ok(Self::Terminated),
            "timeout" | "timed_out" => Ok(Self::TimedOut),
            "budget_exceeded" => Ok(Self::BudgetExceeded),
            "unenforceable" => Ok(Self::Unenforceable),
            other => Err(Error::validation(format!(
                "sandbox: unknown exit reason {other:?}"
            ))),
        }
    }
}

/// The final outcome of one session.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ExitStatus {
    /// Exit code, when the process exited normally.
    pub code: Option<i32>,
    /// Terminating signal, when the OS reported one.
    pub signal: Option<i32>,
    /// The classification.
    pub reason: ExitReason,
}

impl ExitStatus {
    /// A normal exit with `code`.
    #[must_use]
    pub fn exited(code: i32) -> Self {
        Self {
            code: Some(code),
            signal: None,
            reason: ExitReason::Exited,
        }
    }

    /// The classified error this outcome implies, if any.
    ///
    /// The outcome itself is always available (a caller usually wants the exit
    /// code), so the classification is exposed as a second question instead of
    /// replacing the answer with an `Err`.
    #[must_use]
    pub fn error(&self) -> Option<Error> {
        match self.reason {
            ExitReason::Exited | ExitReason::Signaled | ExitReason::Terminated => None,
            ExitReason::TimedOut => Some(Error::timeout("sandbox: session timed out")),
            ExitReason::BudgetExceeded => Some(Error::budget_exceeded(
                "sandbox: session exceeded its resource budget",
            )),
            ExitReason::Unenforceable => Some(Error::not_available(
                "sandbox: resource limits could not be enforced",
            )),
        }
    }
}

/// One contiguous run of bytes from one stream, starting at [`Chunk::seq`].
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Chunk {
    /// Byte cursor of the first byte in this chunk.
    pub seq: u64,
    /// Which stream it came from.
    pub stream: Stream,
    /// Payload.
    pub data: Vec<u8>,
}

/// One pull read.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ReadResult {
    /// Cursor to pass as `after_seq` next time.
    pub next_seq: u64,
    /// Chunks after the requested cursor.
    pub chunks: Vec<Chunk>,
    /// Whether the session ended and nothing remains after `next_seq`.
    pub eof: bool,
}

/// The session features a runner or a session actually provides.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct SessionFeatures {
    /// A tty (pty) is available, so `resize` works and output is merged.
    pub tty: bool,
    /// A session has an input side to write to.
    pub stdin: bool,
    /// Named signals can be delivered.
    pub signal: bool,
    /// [`Session::watch`] delivers pushed events.
    pub events: bool,
}

/// The runner-level feature declaration (an alias kept for parity with the
/// wire's `Features`).
pub type Features = SessionFeatures;

/// Where the host looks for programmes.
///
/// A bare `argv[0]` (`git`, not `/usr/bin/git`) is resolved against this list and
/// nothing else: the request's `PATH` belongs to the child's environment, so a
/// resolution that consulted it would let the caller choose which file a rule is
/// judged against.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct HostPolicy {
    /// The directories a bare `argv[0]` is resolved against, in order: the first
    /// executable match wins.
    pub path_dirs: Vec<PathBuf>,
}

impl Default for HostPolicy {
    fn default() -> Self {
        Self {
            path_dirs: ["/usr/local/bin", "/usr/bin", "/bin"]
                .iter()
                .map(PathBuf::from)
                .collect(),
        }
    }
}

/// Build a [`HostPolicy`] from a deployment's list, refusing what cannot be resolved.
///
/// Absolute only: a relative directory would resolve against the daemon's working
/// directory, which is a property of how it was started rather than of the document
/// that named it.
pub fn host_policy_from(dirs: Option<&[String]>) -> Result<HostPolicy> {
    let Some(dirs) = dirs else {
        return Ok(HostPolicy::default());
    };
    if dirs.is_empty() {
        return Err(Error::validation(
            "sandbox: command_path_dirs must not be empty (omit it for the default PATH)",
        ));
    }
    let mut path_dirs = Vec::with_capacity(dirs.len());
    for (index, dir) in dirs.iter().enumerate() {
        if dir.trim().is_empty() {
            return Err(Error::validation(format!(
                "sandbox: command_path_dirs[{index}] is empty"
            )));
        }
        let path = PathBuf::from(dir);
        if !path.is_absolute() {
            return Err(Error::validation(format!(
                "sandbox: command_path_dirs[{index}] {dir:?} is not absolute"
            )));
        }
        path_dirs.push(path);
    }
    Ok(HostPolicy { path_dirs })
}

/// What a runner is holding right now.
///
/// The numbers an operator needs to see a bound approaching *before* a start is
/// refused: a limit with no reading is a limit nobody can plan against.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, serde::Serialize)]
pub struct RunnerStats {
    /// Sessions that are running, or still being started.
    pub sessions_ready: usize,
    /// Exited sessions still listed so their tail can be collected.
    pub sessions_exited: usize,
    /// Retained output across every listed session, in bytes.
    pub log_bytes: u64,
}

/// One pushed session event.
///
/// An enum rather than a struct with a type tag: the payload only exists for
/// the variant that carries it, so an impossible combination cannot be built.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Event {
    /// One output chunk.
    Output {
        /// Byte cursor of the chunk's first byte.
        seq: u64,
        /// Which stream it came from.
        stream: Stream,
        /// Payload.
        ///
        /// Shared rather than owned: one chunk is handed to every watcher, and
        /// to the retained record that a later `read` or `watch` replays. A
        /// `Vec` here meant a copy per subscriber per chunk, on data the log had
        /// already copied once.
        data: Arc<[u8]>,
    },
    /// The process ended; `seq` is the completion cursor.
    Exited {
        /// Cursor after the last byte the process produced.
        seq: u64,
        /// The outcome.
        exit: ExitStatus,
    },
    /// The session was closed.
    Closed {
        /// Completion cursor.
        seq: u64,
    },
    /// The subscriber fell behind; re-read from `next_seq`.
    Lag {
        /// First cursor the subscriber must recover with `read`.
        next_seq: u64,
    },
}

impl Event {
    /// The cursor this event refers to.
    #[must_use]
    pub const fn seq(&self) -> u64 {
        match self {
            Self::Output { seq, .. } | Self::Exited { seq, .. } | Self::Closed { seq } => *seq,
            Self::Lag { next_seq } => *next_seq,
        }
    }
}

/// A push stream of [`Event`]s.
pub type EventStream = Pin<Box<dyn EventSource<Item = Event> + Send + 'static>>;

/// A snapshot of one managed session, for `list`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SessionInfo {
    /// Session id.
    pub id: String,
    /// Command line.
    pub argv: Vec<String>,
    /// Whether it runs on a pty.
    pub tty: bool,
    /// Process id, when known.
    pub pid: Option<u32>,
    /// Start time, nanoseconds since the epoch.
    pub started_at_unix_nano: u64,
    /// What this session declared.
    pub features: SessionFeatures,
    /// Whether the process is still running.
    pub running: bool,
    /// The outcome, once finished.
    pub exit: Option<ExitStatus>,
}

// ---------------------------------------------------------------------------
// Capabilities
// ---------------------------------------------------------------------------

/// The network modes a backend can enforce.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct NetModes {
    /// [`NetMode::DenyAll`] is enforced.
    pub deny_all: bool,
    /// [`NetMode::AllowList`] is enforced.
    pub allow_list: bool,
    /// [`NetMode::Proxy`] is enforced.
    pub proxy: bool,
}

/// The write modes a backend can enforce.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct WriteModes {
    /// Writes are confined to the runner's writable area.
    pub workspace: bool,
    /// The runner root can be kept read-only for one call.
    pub read_only: bool,
}

/// The file-journal surface of one runner.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct JournalCapabilities {
    /// A journal can be opened.
    pub enabled: bool,
    /// Events can name the session that wrote them.
    pub writer_id: bool,
    /// A rename is reported as one event with its origin.
    pub rename_pairing: bool,
    /// Events carry the filesystem identity (device/inode).
    pub file_identity: bool,
    /// How many watches this backend can hold (`0` = unknown).
    pub watch_budget: i32,
}

/// The policy dimensions a backend can enforce.
///
/// Conservative `false` means "not enforced", never "unknown": a caller reads
/// this to decide what it may ask for, so an optimistic `true` is a security
/// bug (see `Capabilities` docs in `core/sandbox` on the Go side).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Enforcement {
    /// The host environment is filtered through an allow-list.
    pub env_allow_list: bool,
    /// Network modes this backend enforces.
    pub net: NetModes,
    /// A socks5 upstream can be dialled.
    pub socks5: bool,
    /// TLS can be terminated for configured hosts.
    pub mitm: bool,
    /// Unix socket egress can be confined to an allow-list.
    pub unix_sockets: bool,
    /// Memory caps are enforced.
    pub memory_cap: bool,
    /// CPU caps are enforced.
    pub cpu_cap: bool,
    /// Disk caps are enforced.
    pub disk_cap: bool,
    /// Writes are confined at the OS level.
    pub filesystem_bounds: bool,
    /// Write modes this backend enforces.
    pub write: WriteModes,
}

/// What a runner can enforce.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Capabilities {
    /// Policy surface.
    pub policy: Enforcement,
    /// Session features.
    pub features: SessionFeatures,
    /// Journal surface.
    pub journal: JournalCapabilities,
}

// ---------------------------------------------------------------------------
// The contract itself
// ---------------------------------------------------------------------------

/// A live session handle.
///
/// Lifecycle, in the order a caller meets it:
///
/// - [`read`](Session::read) blocks until output is available, the process
///   ended, or the returned future is dropped. `after_seq` is a **byte**
///   cursor, so a reader may resume at any offset it has already seen. Output
///   survives [`wait`](Session::wait) and stays replayable until
///   [`close`](Session::close); a cursor the bounded buffer already dropped is
///   an `Aborted` error, never a silent hole.
/// - [`write`](Session::write) sends bytes to the child, or reports
///   `NotAvailable` when the spec never declared an input side.
/// - [`terminate`](Session::terminate) sends `SIGTERM` to the process group,
///   then `SIGKILL` after a short grace period. It is idempotent on an exited
///   process.
/// - [`close`](Session::close) terminates a running session, releases the
///   output log, and makes every later call report `Closed`.
#[async_trait]
pub trait Session: Send + Sync + std::fmt::Debug {
    /// The session id.
    fn id(&self) -> &str;

    /// Process id, when known.
    fn pid(&self) -> Option<u32>;

    /// What this session actually provides.
    fn features(&self) -> SessionFeatures;

    /// The outcome, once the process is gone and reaped.
    ///
    /// Answers without waiting, unlike [`wait`](Session::wait); `list` uses it
    /// to report whether a session still runs.
    fn exit(&self) -> Option<ExitStatus>;

    /// Pull buffered output after `after_seq`.
    async fn read(&self, after_seq: u64, max_bytes: usize) -> Result<ReadResult>;

    /// Write to stdin.
    async fn write(&self, data: &[u8]) -> Result<()>;

    /// Close stdin.
    async fn close_input(&self) -> Result<()>;

    /// Resize the pty, defaulting the window from [`SessionSpec::window`].
    async fn resize(&self, rows: u16, cols: u16) -> Result<()>;

    /// Deliver a soft signal.
    async fn signal(&self, signal: Signal) -> Result<()>;

    /// Stop the session's process group.
    async fn terminate(&self) -> Result<()>;

    /// Wait for the process to end.
    async fn wait(&self) -> Result<ExitStatus>;

    /// Subscribe to pushed events: a replay of the retained output, then live
    /// delivery, then a [`Event::Lag`] if this subscriber falls behind.
    fn watch(&self) -> Result<EventStream>;

    /// Release the session.
    async fn close(&self) -> Result<()>;
}

/// A runner: process sessions under a policy.
#[async_trait]
pub trait Runner: Send + Sync + std::fmt::Debug {
    /// What this runner can enforce.
    ///
    /// Async because a remote runner has to ask its server. It must not fail
    /// into a default: an empty capability set would silently downgrade the
    /// caller's expectations.
    fn capabilities(&self) -> crate::resource::BoxFuture<'_, Result<Capabilities>>;

    /// Start a session.
    async fn start(&self, spec: SessionSpec) -> Result<Arc<dyn Session>>;

    /// Look up a live session.
    async fn session(&self, id: &str) -> Result<Arc<dyn Session>>;

    /// Every known session.
    async fn list(&self) -> Result<Vec<SessionInfo>>;

    /// Terminate one session by id.
    async fn terminate(&self, id: &str) -> Result<()>;

    /// Release one session: terminate it if it still runs, free its output, and
    /// forget it so it disappears from [`list`](Runner::list).
    async fn close_session(&self, id: &str) -> Result<()>;

    /// The directories a bare command name is resolved against.
    ///
    /// The runner is the one that knows: it is built from the deployment's
    /// settings, and every layer that judges a command (the command gate, the
    /// read-only classifier, the grant's scope check) has to resolve `git` the same
    /// way or they judge different files.
    ///
    /// The default is the conventional host PATH, for a runner assembled in code.
    fn host_policy(&self) -> HostPolicy {
        HostPolicy::default()
    }

    /// What this runner is holding, for the daemon's status surface.
    ///
    /// Synchronous and cheap by design: it is read on every `/status`, and a
    /// counter that needs an await would make the check as expensive as the work
    /// it describes.
    ///
    /// `None` means **not reported**, which is not the same statement as zero: a
    /// remote runner has no local table to count, and a default of zeros would
    /// make it look idle.
    fn stats(&self) -> Option<RunnerStats> {
        None
    }

    /// Open the file journal of this runner.
    ///
    /// The default is [`ErrorKind::NotAvailable`](crate::error::ErrorKind::NotAvailable):
    /// [`Capabilities::journal`] says whether asking makes sense, and a decorator
    /// must forward this rather than swallow it, or the declaration becomes a
    /// lie.
    async fn open_journal(&self) -> Result<Arc<dyn crate::caps::sandbox::journal::Journal>> {
        Err(Error::not_available(
            "sandbox: this runner has no file journal",
        ))
    }

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
    fn shutdown(self: Arc<Self>) -> crate::resource::BoxFuture<'static, Result<()>> {
        Box::pin(async move { self.0.close().await })
    }
}

#[cfg(test)]
mod host_policy_tests {
    use super::*;

    /// A deployment whose tools live off the conventional PATH has to be able to say
    /// so: bare rules resolve against *this* list, and the wrong one means either
    /// "nothing resolves" (fail closed, confusing) or "the wrong file resolves".
    #[test]
    fn a_deployment_may_name_its_own_programme_directories() {
        assert_eq!(
            host_policy_from(None).expect("default"),
            HostPolicy::default()
        );

        // Built from a temporary directory rather than a literal POSIX path:
        // "absolute" is a platform question (`/opt/tools` is neither absolute nor
        // usable on Windows), and this test is about the plumbing, not about one
        // host's layout.
        let home = tempfile::tempdir().expect("tempdir");
        let first = home.path().join("bin");
        let second = home.path().join("sbin");
        let named = host_policy_from(Some(&[first.display().to_string()])).expect("named");
        assert_eq!(named.path_dirs, vec![first.clone()]);

        // The order is the precedence, so it is preserved.
        let ordered = host_policy_from(Some(&[
            first.display().to_string(),
            second.display().to_string(),
        ]))
        .expect("ordered");
        assert_eq!(ordered.path_dirs, vec![first, second]);
    }

    #[test]
    fn a_directory_that_cannot_be_resolved_predictably_is_refused() {
        // Relative would resolve against the daemon's working directory — a property
        // of how it was started, not of the document.
        assert!(host_policy_from(Some(&["tools".to_owned()])).is_err());
        assert!(host_policy_from(Some(&["".to_owned()])).is_err());
        assert!(host_policy_from(Some(&[])).is_err());
    }
}

#[cfg(test)]
mod env_policy_tests {
    use super::*;

    fn names(values: &[&str]) -> Vec<String> {
        values.iter().map(|value| (*value).to_owned()).collect()
    }

    fn policy(allow: Option<&[&str]>) -> EnvPolicy {
        EnvPolicy {
            allow: allow.map(names),
            inject: BTreeMap::new(),
        }
    }

    #[test]
    fn a_provider_without_a_policy_leaves_the_request_alone() {
        let mut requested = policy(Some(&["HOME"]));
        requested.tighten(None);
        assert_eq!(requested.allow, Some(vec!["HOME".to_owned()]));
    }

    #[test]
    fn a_request_that_asked_for_everything_is_bounded_by_the_provider() {
        let mut requested = policy(None);
        assert!(requested.inherits_host());
        requested.tighten(Some(&names(&["PATH", "HOME"])));
        assert_eq!(
            requested.allow,
            Some(vec!["PATH".to_owned(), "HOME".to_owned()])
        );
        assert!(!requested.inherits_host());
    }

    #[test]
    fn a_request_that_asked_for_less_stays_bounded_both_ways() {
        let mut requested = policy(Some(&["HOME", "SECRET"]));
        requested.tighten(Some(&names(&["HOME", "PATH"])));
        assert_eq!(
            requested.allow,
            Some(vec!["HOME".to_owned()]),
            "the intersection, in the caller's order"
        );
        // An empty provider list is the strictest deployment there is.
        let mut requested = policy(Some(&["HOME"]));
        requested.tighten(Some(&[]));
        assert_eq!(requested.allow, Some(Vec::new()));
    }

    #[test]
    fn a_name_that_is_not_a_variable_name_is_refused() {
        for names in [
            vec![String::new()],
            vec!["A=B".to_owned()],
            vec![" HOME".to_owned()],
        ] {
            assert!(
                EnvPolicy::validate_allow_names(&names).is_err(),
                "{names:?} must be refused"
            );
        }
        assert!(EnvPolicy::validate_allow_names(&["PATH".to_owned()]).is_ok());
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn net_modes_round_trip_and_reject_unknown() {
        for mode in [
            NetMode::Default,
            NetMode::DenyAll,
            NetMode::AllowList,
            NetMode::Proxy,
        ] {
            assert_eq!(NetMode::parse(mode.as_str()).unwrap(), mode);
        }
        assert_eq!(NetMode::parse("").unwrap(), NetMode::Default);
        assert_eq!(
            NetMode::parse("allowlist").unwrap_err().kind(),
            crate::error::ErrorKind::Validation
        );
    }

    #[test]
    fn write_modes_accept_the_legacy_spelling() {
        assert_eq!(WriteMode::parse("default").unwrap(), WriteMode::Workspace);
        assert_eq!(WriteMode::parse("read-only").unwrap(), WriteMode::ReadOnly);
        assert!(WriteMode::parse("write").is_err());
    }

    #[test]
    fn exit_reasons_accept_the_legacy_spelling_and_carry_an_error() {
        assert_eq!(ExitReason::parse("timeout").unwrap(), ExitReason::TimedOut);
        assert_eq!(ExitReason::parse("signal").unwrap(), ExitReason::Signaled);
        assert!(ExitStatus::exited(0).error().is_none());
        let timed_out = ExitStatus {
            code: None,
            signal: Some(9),
            reason: ExitReason::TimedOut,
        };
        assert_eq!(
            timed_out.error().unwrap().kind(),
            crate::error::ErrorKind::Timeout
        );
    }

    #[test]
    fn a_proxy_url_is_validated_on_its_own() {
        // Go accepts an upstream in both allow-list and proxy mode, so the field
        // is checked for form, not for a mode it happens to appear with.
        let policy = NetPolicy {
            mode: NetMode::AllowList,
            proxy: Some("http://127.0.0.1:3128".to_owned()),
            ..NetPolicy::default()
        };
        assert!(policy.validate().is_ok());

        let policy = NetPolicy {
            mode: NetMode::Proxy,
            proxy: Some("ftp://127.0.0.1:3128".to_owned()),
            ..NetPolicy::default()
        };
        assert!(policy.validate().is_err());

        let policy = NetPolicy {
            mode: NetMode::Proxy,
            proxy: Some("socks5://user:pass@127.0.0.1:1080".to_owned()),
            ..NetPolicy::default()
        };
        policy.validate().unwrap();
    }

    #[test]
    fn an_empty_window_defaults_to_24x80() {
        let spec = SessionSpec::new(vec!["true".to_owned()]);
        assert_eq!(spec.window(), (24, 80));
        assert!(!spec.wants_stdin());

        let mut tty = spec.clone();
        tty.tty = true;
        tty.rows = 40;
        assert_eq!(tty.window(), (40, 80));
        assert!(tty.wants_stdin(), "a pty is always writable");
    }
}
