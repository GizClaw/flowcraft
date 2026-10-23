//! The local runner: processes on this host, no OS confinement.
//!
//! What it does enforce: an environment allow-list, a working directory, a
//! timeout that kills the process group, and a bounded output buffer. What it
//! does **not** enforce: filesystem confinement, network policy, memory/CPU/
//! disk caps, TTY, or named signals. Those are reported honestly by
//! [`Runner::capabilities`], and a spec that asks for one is rejected rather
//! than silently downgraded.

use std::collections::{BTreeMap, VecDeque};
use std::process::Stdio;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{SystemTime, UNIX_EPOCH};

use async_trait::async_trait;
use flowcraft_error::{Error, Result};
use flowcraft_resource::Kind;
use flowcraft_resource::{BoxFuture, BuildCtx, Factory, Phase, Spec};
use serde::Deserialize;
use serde_json::Value;
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWriteExt};
use tokio::process::{Child, ChildStdin, Command};
use tokio::sync::Mutex as AsyncMutex;

use crate::contract::{
    CONTRACT, Capabilities, Chunk, Enforcement, ExitStatus, Features, Handle, KIND, ReadResult,
    Runner, Session, SessionFeatures, SessionInfo, SessionSpec, WriteMode,
};

const DEFAULT_OUTPUT_LIMIT: u64 = 1 << 20;
const READ_BATCH_LIMIT: usize = 1 << 20;

/// A runner that spawns processes on this host.
pub struct LocalRunner {
    sessions: AsyncMutex<BTreeMap<String, Arc<LocalSession>>>,
    counter: AtomicU64,
    default_output_limit: u64,
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
    /// A runner with no sessions.
    #[must_use]
    pub fn new() -> Self {
        Self {
            sessions: AsyncMutex::new(BTreeMap::new()),
            counter: AtomicU64::new(1),
            default_output_limit: DEFAULT_OUTPUT_LIMIT,
        }
    }

    /// The output cap for sessions that do not declare one.
    #[must_use]
    pub fn with_output_limit(mut self, bytes: u64) -> Self {
        self.default_output_limit = bytes.max(1);
        self
    }
}

/// One local process session.
impl std::fmt::Debug for LocalSession {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("LocalSession")
            .field("id", &self.id)
            .field("pid", &self.pid)
            .finish_non_exhaustive()
    }
}

struct LocalSession {
    id: String,
    argv: Vec<String>,
    pid: Option<u32>,
    started_at: u64,
    features: SessionFeatures,
    log: Mutex<Log>,
    child: AsyncMutex<Option<Child>>,
    stdin: AsyncMutex<Option<ChildStdin>>,
    exit: Mutex<Option<ExitStatus>>,
    /// Why the session is being stopped, set before the kill. `wait` and
    /// `terminate` both reap the child, so the reason has to be decided by
    /// whoever asked to stop it, not by whoever wins the race to `wait`.
    pending_reason: Mutex<Option<String>>,
    /// Output pumps. They are joined before the log is marked ended, so the
    /// last chunks of stdout/stderr are never lost to a race with `wait`.
    pumps: Mutex<Vec<tokio::task::JoinHandle<()>>>,
}

struct Log {
    chunks: VecDeque<Chunk>,
    next_seq: u64,
    bytes: u64,
    limit: u64,
    ended: bool,
}

impl Log {
    fn new(limit: u64) -> Self {
        Self {
            chunks: VecDeque::new(),
            next_seq: 1,
            bytes: 0,
            limit,
            ended: false,
        }
    }

    fn push(&mut self, stream: &str, data: &[u8]) {
        if self.ended || data.is_empty() {
            return;
        }
        let remaining = self.limit.saturating_sub(self.bytes);
        if remaining == 0 {
            return;
        }
        let take = usize::try_from(remaining)
            .unwrap_or(usize::MAX)
            .min(data.len());
        let seq = self.next_seq;
        self.next_seq += 1;
        self.bytes += take as u64;
        self.chunks.push_back(Chunk {
            seq,
            stream: stream.to_owned(),
            data: data[..take].to_vec(),
        });
    }

    fn read(&self, after_seq: u64, max_bytes: usize) -> ReadResult {
        let max_bytes = max_bytes.min(READ_BATCH_LIMIT);
        let mut chunks = Vec::new();
        let mut bytes = 0usize;
        let mut next_seq = after_seq;
        for chunk in &self.chunks {
            if chunk.seq <= after_seq {
                continue;
            }
            if bytes + chunk.data.len() > max_bytes && !chunks.is_empty() {
                break;
            }
            bytes += chunk.data.len();
            next_seq = chunk.seq;
            chunks.push(chunk.clone());
        }
        let more = self.chunks.iter().any(|chunk| chunk.seq > next_seq);
        ReadResult {
            next_seq,
            chunks,
            eof: self.ended && !more,
        }
    }
}

fn now_nanos() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |duration| duration.as_nanos() as u64)
}

#[cfg(unix)]
fn kill_group(pid: u32) {
    use nix::sys::signal::{Signal, killpg};
    use nix::unistd::Pid;

    if let Ok(raw) = i32::try_from(pid) {
        let _ = killpg(Pid::from_raw(raw), Signal::SIGKILL);
    }
}

#[cfg(not(unix))]
fn kill_group(_pid: u32) {}

impl LocalSession {
    /// Wait for both output pumps to reach EOF, so no chunk is dropped when the
    /// log is marked ended.
    async fn settle(&self) {
        let handles: Vec<tokio::task::JoinHandle<()>> = self
            .pumps
            .lock()
            .map(|mut pumps| std::mem::take(&mut *pumps))
            .unwrap_or_default();
        for handle in handles {
            let _ = handle.await;
        }
    }

    async fn terminate_reason(&self, reason: &str) -> Result<ExitStatus> {
        if let Some(exit) = self.exit.lock().map_err(|_| poisoned())?.clone() {
            return Ok(exit);
        }
        if let Ok(mut pending) = self.pending_reason.lock() {
            *pending = Some(reason.to_owned());
        }
        if let Some(pid) = self.pid {
            kill_group(pid);
        }
        let mut guard = self.child.lock().await;
        let mut exit = if let Some(child) = guard.as_mut() {
            let status = child.wait().await.map_err(Error::from)?;
            status_of(status)
        } else {
            ExitStatus::default()
        };
        exit.reason = self
            .pending_reason
            .lock()
            .ok()
            .and_then(|pending| pending.clone())
            .unwrap_or_else(|| reason.to_owned());
        *self.exit.lock().map_err(|_| poisoned())? = Some(exit.clone());
        self.settle().await;
        if let Ok(mut log) = self.log.lock() {
            log.ended = true;
        }
        Ok(exit)
    }
}

fn poisoned() -> Error {
    Error::internal("sandbox session lock poisoned")
}

fn status_of(status: std::process::ExitStatus) -> ExitStatus {
    #[cfg(unix)]
    {
        use std::os::unix::process::ExitStatusExt;
        if let Some(signal) = status.signal() {
            return ExitStatus {
                code: None,
                signal: Some(signal),
                reason: "signal".to_owned(),
            };
        }
    }
    ExitStatus {
        code: status.code(),
        signal: None,
        reason: "exited".to_owned(),
    }
}

#[async_trait]
impl Session for LocalSession {
    fn id(&self) -> &str {
        &self.id
    }

    fn pid(&self) -> Option<u32> {
        self.pid
    }

    fn features(&self) -> SessionFeatures {
        self.features
    }

    async fn read(&self, after_seq: u64, max_bytes: usize) -> Result<ReadResult> {
        Ok(self
            .log
            .lock()
            .map_err(|_| poisoned())?
            .read(after_seq, max_bytes))
    }

    async fn write(&self, data: &[u8]) -> Result<()> {
        let mut guard = self.stdin.lock().await;
        let stdin = guard.as_mut().ok_or_else(|| {
            Error::not_available("session was started without stdin; set spec.stdin")
        })?;
        stdin.write_all(data).await.map_err(Error::from)?;
        stdin.flush().await.map_err(Error::from)?;
        Ok(())
    }

    async fn close_input(&self) -> Result<()> {
        let mut guard = self.stdin.lock().await;
        if let Some(mut stdin) = guard.take() {
            let _ = stdin.shutdown().await;
        }
        Ok(())
    }

    async fn wait(&self) -> Result<ExitStatus> {
        if let Some(exit) = self.exit.lock().map_err(|_| poisoned())?.clone() {
            return Ok(exit);
        }
        let mut guard = self.child.lock().await;
        if let Some(exit) = self.exit.lock().map_err(|_| poisoned())?.clone() {
            return Ok(exit);
        }
        let mut exit = if let Some(child) = guard.as_mut() {
            let status = child.wait().await.map_err(Error::from)?;
            status_of(status)
        } else {
            ExitStatus::default()
        };
        if let Some(reason) = self
            .pending_reason
            .lock()
            .ok()
            .and_then(|pending| pending.clone())
        {
            exit.reason = reason;
        }
        *self.exit.lock().map_err(|_| poisoned())? = Some(exit.clone());
        self.settle().await;
        if let Ok(mut log) = self.log.lock() {
            log.ended = true;
        }
        Ok(exit)
    }

    async fn terminate(&self) -> Result<()> {
        self.terminate_reason("terminated").await?;
        Ok(())
    }
}

#[async_trait]
impl Runner for LocalRunner {
    fn capabilities(&self) -> BoxFuture<'_, Result<Capabilities>> {
        Box::pin(async move {
            Ok(Capabilities {
                policy: Enforcement {
                    env_allow_list: true,
                    net_modes: vec!["default".to_owned()],
                    filesystem_bounds: false,
                    write_modes: vec!["default".to_owned()],
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
            })
        })
    }

    async fn start(&self, spec: SessionSpec) -> Result<Arc<dyn Session>> {
        if spec.argv.is_empty() {
            return Err(Error::validation("sandbox: argv is empty"));
        }
        if spec.tty {
            return Err(Error::not_available(
                "sandbox: the local runner has no TTY; use a backend that declares tty",
            ));
        }
        if !spec.opts.net.mode.is_empty() && spec.opts.net.mode != "default" {
            return Err(Error::not_available(format!(
                "sandbox: the local runner cannot enforce net mode {:?}",
                spec.opts.net.mode
            )));
        }
        if spec.opts.write == WriteMode::ReadOnly {
            return Err(Error::not_available(
                "sandbox: the local runner has no filesystem boundary, so read-only writes cannot be enforced",
            ));
        }
        if spec.opts.limits.cpu_millicores.is_some()
            || spec.opts.limits.memory_bytes.is_some()
            || spec.opts.limits.disk_bytes.is_some()
        {
            return Err(Error::not_available(
                "sandbox: the local runner cannot enforce cpu/memory/disk limits",
            ));
        }
        if let Some(dir) = &spec.opts.workdir
            && !std::path::Path::new(dir).is_dir()
        {
            return Err(Error::not_found(format!(
                "sandbox: workdir {dir:?} does not exist"
            )));
        }

        let id = if spec.id.trim().is_empty() {
            format!(
                "s-{}-{}",
                std::process::id(),
                self.counter.fetch_add(1, Ordering::Relaxed)
            )
        } else {
            spec.id.clone()
        };
        {
            let sessions = self.sessions.lock().await;
            if sessions.contains_key(&id) {
                return Err(Error::conflict(format!(
                    "sandbox: session {id:?} already exists"
                )));
            }
        }

        let mut command = Command::new(&spec.argv[0]);
        command.args(&spec.argv[1..]);
        let wants_stdin = spec.stdin
            || spec
                .opts
                .stdin
                .as_ref()
                .is_some_and(|bytes| !bytes.is_empty());
        command.stdin(if wants_stdin {
            Stdio::piped()
        } else {
            Stdio::null()
        });
        command.stdout(Stdio::piped());
        command.stderr(Stdio::piped());
        command.env_clear();
        for name in &spec.opts.env.allow {
            if let Ok(value) = std::env::var(name) {
                command.env(name, value);
            }
        }
        for (name, value) in &spec.opts.env.inject {
            command.env(name, value);
        }
        if let Some(dir) = &spec.opts.workdir {
            command.current_dir(dir);
        }
        #[cfg(unix)]
        command.process_group(0);

        let mut child = command.spawn().map_err(|error| {
            let kind = match error.kind() {
                std::io::ErrorKind::NotFound => flowcraft_error::ErrorKind::NotFound,
                std::io::ErrorKind::PermissionDenied => flowcraft_error::ErrorKind::Forbidden,
                _ => flowcraft_error::ErrorKind::NotAvailable,
            };
            Error::new(
                kind,
                format!("sandbox: cannot start {:?}: {error}", spec.argv[0]),
            )
        })?;
        let pid = child.id();
        let stdout = child.stdout.take();
        let stderr = child.stderr.take();
        let stdin = child.stdin.take();

        let session = Arc::new(LocalSession {
            id: id.clone(),
            argv: spec.argv.clone(),
            pid,
            started_at: now_nanos(),
            features: SessionFeatures {
                tty: false,
                stdin: wants_stdin,
                signal: false,
                events: false,
            },
            log: Mutex::new(Log::new(
                spec.opts
                    .limits
                    .max_output_bytes
                    .unwrap_or(self.default_output_limit),
            )),
            child: AsyncMutex::new(Some(child)),
            stdin: AsyncMutex::new(stdin),
            exit: Mutex::new(None),
            pending_reason: Mutex::new(None),
            pumps: Mutex::new(Vec::new()),
        });

        if let Some(reader) = stdout {
            let handle = tokio::spawn(pump(session.clone(), reader, "stdout"));
            session.pumps.lock().map_err(|_| poisoned())?.push(handle);
        }
        if let Some(reader) = stderr {
            let handle = tokio::spawn(pump(session.clone(), reader, "stderr"));
            session.pumps.lock().map_err(|_| poisoned())?.push(handle);
        }
        if let Some(bytes) = &spec.opts.stdin
            && !bytes.is_empty()
        {
            let mut guard = session.stdin.lock().await;
            if let Some(stdin) = guard.as_mut() {
                stdin.write_all(bytes).await.map_err(Error::from)?;
                stdin.flush().await.map_err(Error::from)?;
            }
        }
        if !spec.stdin {
            session.close_input().await?;
        }
        if let Some(timeout) = spec.opts.timeout {
            let watchdog = session.clone();
            tokio::spawn(async move {
                tokio::time::sleep(timeout).await;
                let finished = watchdog
                    .exit
                    .lock()
                    .map(|exit| exit.is_some())
                    .unwrap_or(true);
                if !finished {
                    let _ = watchdog.terminate_reason("timeout").await;
                }
            });
        }

        self.sessions
            .lock()
            .await
            .insert(id.clone(), session.clone());
        Ok(session)
    }

    async fn session(&self, id: &str) -> Result<Arc<dyn Session>> {
        let sessions = self.sessions.lock().await;
        match sessions.get(id) {
            Some(session) => Ok(session.clone()),
            None => Err(Error::not_found(format!("sandbox: session {id:?}"))),
        }
    }

    async fn list(&self) -> Result<Vec<SessionInfo>> {
        let sessions = self.sessions.lock().await;
        let mut infos = Vec::with_capacity(sessions.len());
        for session in sessions.values() {
            let exit = session.exit.lock().map_err(|_| poisoned())?.clone();
            infos.push(SessionInfo {
                id: session.id.clone(),
                argv: session.argv.clone(),
                pid: session.pid,
                started_at_unix_nano: session.started_at,
                features: session.features,
                running: exit.is_none(),
                exit,
            });
        }
        Ok(infos)
    }

    async fn terminate(&self, id: &str) -> Result<()> {
        let session = self.session(id).await?;
        session.terminate().await
    }

    async fn close(&self) -> Result<()> {
        let sessions: Vec<Arc<dyn Session>> = {
            let guard = self.sessions.lock().await;
            guard
                .values()
                .map(|session| session.clone() as Arc<dyn Session>)
                .collect()
        };
        for session in sessions {
            let _ = session.terminate().await;
        }
        self.sessions.lock().await.clear();
        Ok(())
    }
}

async fn pump(
    session: Arc<LocalSession>,
    mut reader: impl AsyncRead + Unpin,
    stream: &'static str,
) {
    let mut buffer = [0u8; 8192];
    loop {
        match reader.read(&mut buffer).await {
            Ok(0) => break,
            Ok(read) => {
                if let Ok(mut log) = session.log.lock() {
                    log.push(stream, &buffer[..read]);
                }
            }
            Err(_) => break,
        }
    }
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct Settings {
    #[serde(default)]
    max_output_bytes: Option<u64>,
}

fn validate(value: &Value) -> Result<()> {
    let _: Settings = flowcraft_resource::settings(value)?;
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
            let mut runner = LocalRunner::new();
            if let Some(limit) = settings.max_output_bytes {
                runner = runner.with_output_limit(limit);
            }
            Ok(Arc::new(Handle::new(Arc::new(runner))))
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::contract::ExecOptions;
    use std::time::Duration;

    fn spec(argv: &[&str]) -> SessionSpec {
        SessionSpec {
            id: String::new(),
            argv: argv.iter().map(|arg| (*arg).to_owned()).collect(),
            tty: false,
            stdin: false,
            opts: ExecOptions::default(),
        }
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
            if read.chunks.is_empty() {
                break;
            }
        }
        out
    }

    #[tokio::test]
    async fn runs_a_command_and_reports_output_and_exit() {
        let runner = LocalRunner::new();
        let session = runner
            .start(spec(&["sh", "-c", "echo hello; exit 3"]))
            .await
            .unwrap();
        let exit = session.wait().await.unwrap();
        assert_eq!(exit.code, Some(3));
        assert_eq!(exit.reason, "exited");
        assert!(read_all(&session).await.contains("hello"));
    }

    #[tokio::test]
    async fn stdin_is_delivered_and_can_be_closed() {
        let runner = LocalRunner::new();
        let mut start = spec(&["cat"]);
        start.stdin = true;
        let session = runner.start(start).await.unwrap();
        session.write(b"ping\n").await.unwrap();
        session.close_input().await.unwrap();
        let exit = session.wait().await.unwrap();
        assert_eq!(exit.code, Some(0));
        assert!(read_all(&session).await.contains("ping"));
    }

    #[tokio::test]
    async fn the_environment_is_an_allow_list() {
        let runner = LocalRunner::new();
        let mut start = spec(&["sh", "-c", "printf '%s' \"${SECRET:-missing}\""]);
        start
            .opts
            .env
            .inject
            .insert("SECRET".to_owned(), "s3cr3t".to_owned());
        let session = runner.start(start).await.unwrap();
        session.wait().await.unwrap();
        assert_eq!(read_all(&session).await, "s3cr3t");
    }

    #[tokio::test]
    async fn the_environment_does_not_leak_by_default() {
        let runner = LocalRunner::new();
        let session = runner
            .start(spec(&["sh", "-c", "printf '%s' \"${HOME:-missing}\""]))
            .await
            .unwrap();
        session.wait().await.unwrap();
        assert_eq!(read_all(&session).await, "missing");
    }

    #[tokio::test]
    async fn timeouts_kill_the_session() {
        let runner = LocalRunner::new();
        let mut start = spec(&["sleep", "30"]);
        start.opts.timeout = Some(Duration::from_millis(100));
        let session = runner.start(start).await.unwrap();
        let exit = session.wait().await.unwrap();
        assert_eq!(exit.reason, "timeout");
    }

    #[tokio::test]
    async fn terminate_kills_a_running_session() {
        let runner = LocalRunner::new();
        let session = runner.start(spec(&["sleep", "30"])).await.unwrap();
        session.terminate().await.unwrap();
        let exit = session.wait().await.unwrap();
        assert_eq!(exit.reason, "terminated");
    }

    #[tokio::test]
    async fn output_is_bounded() {
        let runner = LocalRunner::new();
        let mut start = spec(&["sh", "-c", "yes x | head -c 100000"]);
        start.opts.limits.max_output_bytes = Some(64);
        let session = runner.start(start).await.unwrap();
        session.wait().await.unwrap();
        let read = read_all(&session).await;
        assert!(read.len() <= 64, "{}", read.len());
    }

    #[tokio::test]
    async fn unsupported_policies_are_refused() {
        let runner = LocalRunner::new();

        let mut tty = spec(&["true"]);
        tty.tty = true;
        assert_eq!(
            runner.start(tty).await.err().unwrap().kind(),
            flowcraft_error::ErrorKind::NotAvailable
        );

        let mut net = spec(&["true"]);
        net.opts.net.mode = "deny-all".to_owned();
        assert_eq!(
            runner.start(net).await.err().unwrap().kind(),
            flowcraft_error::ErrorKind::NotAvailable
        );

        let mut write = spec(&["true"]);
        write.opts.write = WriteMode::ReadOnly;
        assert_eq!(
            runner.start(write).await.err().unwrap().kind(),
            flowcraft_error::ErrorKind::NotAvailable
        );

        let mut memory = spec(&["true"]);
        memory.opts.limits.memory_bytes = Some(1024);
        assert_eq!(
            runner.start(memory).await.err().unwrap().kind(),
            flowcraft_error::ErrorKind::NotAvailable
        );
    }

    #[tokio::test]
    async fn a_missing_command_is_not_found() {
        let runner = LocalRunner::new();
        let error = runner
            .start(spec(&["flowcraft-command-that-does-not-exist"]))
            .await
            .err()
            .unwrap();
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::NotFound);
    }

    #[tokio::test]
    async fn sessions_are_listed_and_addressable() {
        let runner = LocalRunner::new();
        let session = runner.start(spec(&["sh", "-c", "exit 0"])).await.unwrap();
        let id = session.id().to_owned();
        assert!(runner.session(&id).await.is_ok());
        let listed = runner.list().await.unwrap();
        assert_eq!(listed.len(), 1);
        assert_eq!(listed[0].id, id);
        session.wait().await.unwrap();
        runner.close().await.unwrap();
        assert_eq!(runner.list().await.unwrap().len(), 0);
    }

    #[tokio::test]
    async fn duplicate_session_ids_are_a_conflict() {
        let runner = LocalRunner::new();
        let mut first = spec(&["sleep", "5"]);
        first.id = "fixed".to_owned();
        let session = runner.start(first).await.unwrap();
        let mut second = spec(&["true"]);
        second.id = "fixed".to_owned();
        assert_eq!(
            runner.start(second).await.err().unwrap().kind(),
            flowcraft_error::ErrorKind::Conflict
        );
        session.terminate().await.unwrap();
    }

    #[tokio::test]
    async fn capabilities_are_honest() {
        let capabilities = LocalRunner::new().capabilities().await.unwrap();
        assert!(capabilities.policy.env_allow_list);
        assert!(!capabilities.policy.filesystem_bounds);
        assert!(!capabilities.features.tty);
        assert!(!capabilities.features.signal);
        assert_eq!(capabilities.policy.net_modes, vec!["default".to_owned()]);
    }
}
