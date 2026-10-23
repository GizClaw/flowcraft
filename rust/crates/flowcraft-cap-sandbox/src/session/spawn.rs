//! The pipe-backed process session.
//!
//! This is the only place that spawns a child directly; the confined backends
//! rewrite the spec and delegate here, so every session in the crate shares the
//! same cursor, termination, and exit-classification semantics.
//!
//! Policy is applied once, at spawn: `read`, `write`, `resize`, and `terminate`
//! never re-negotiate it. A request for something this backend cannot enforce
//! is refused with `NotAvailable` rather than silently downgraded — the caller
//! asked for a guarantee, and running without it would be a lie.

use std::process::Stdio;
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::Duration;

use async_trait::async_trait;
use flowcraft_error::{Error, Result};
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWriteExt};
use tokio::process::{ChildStdin, Command};
use tokio::sync::{Mutex as AsyncMutex, watch};

use crate::contract::{
    EnvPolicy, EventStream, ExitReason, ExitStatus, ReadResult, Session, SessionFeatures,
    SessionSpec, Signal, Stream,
};
use crate::limits::{Armed, Manager};
use crate::session::log::Log;

/// Output retained when the spec declares no bound (10 MiB, as the Go side).
pub(crate) const DEFAULT_OUTPUT_LIMIT: u64 = 10 * 1024 * 1024;

/// How one backend spawns: its filesystem boundary and its output budget.
pub(crate) struct SpawnOptions {
    /// The runner root. `None` means "no root to check against" — the confined
    /// backends resolve the workdir themselves and hand over a plain path.
    pub(crate) root: Option<std::path::PathBuf>,
    /// Output budget for sessions that declare none.
    pub(crate) default_output_limit: u64,
    /// What this host can enforce for cpu and memory.
    pub(crate) limits: std::sync::Arc<Manager>,
}

/// How long a `SIGTERM` gets before the group is killed outright.
pub(crate) const TERMINATE_GRACE: Duration = Duration::from_secs(2);

/// How often the supervisor checks the deadline and the resource budgets.
///
/// Fast enough to catch a runaway build, slow enough to be free: one read of two
/// small files per tick, only for sessions that asked for a cap.
const SUPERVISE_TICK: Duration = Duration::from_millis(50);

/// How much one pump reads per call.
const PUMP_BUFFER: usize = 8 * 1024;

fn poisoned() -> Error {
    Error::internal("sandbox: session lock poisoned")
}

fn closed_error() -> Error {
    Error::closed("sandbox: session is closed")
}

#[cfg(unix)]
fn signal_group(pid: u32, signal: GroupSignal) {
    use nix::sys::signal::{Signal as Unix, killpg};
    use nix::unistd::Pid;

    let unix = match signal {
        GroupSignal::Interrupt => Unix::SIGINT,
        GroupSignal::Terminate => Unix::SIGTERM,
        GroupSignal::Kill => Unix::SIGKILL,
    };
    if let Ok(raw) = i32::try_from(pid) {
        let _ = killpg(Pid::from_raw(raw), unix);
    }
}

#[cfg(not(unix))]
fn signal_group(_pid: u32, _signal: GroupSignal) {}

/// One session backed by a real child process.
pub(crate) struct ProcessSession {
    id: String,
    pid: Option<u32>,
    features: SessionFeatures,
    log: Arc<Log>,
    stdin: AsyncMutex<Option<ChildStdin>>,
    exited: watch::Receiver<Option<ExitStatus>>,
    /// Why the session is being stopped, decided by whoever asked for it. The
    /// reaper reads it so the reason does not depend on who wins the race to
    /// `wait`.
    pending: Arc<Mutex<Option<ExitReason>>>,
    closed: Mutex<bool>,
    /// Set the moment `child.wait()` returns, before the pumps are joined.
    ///
    /// The exit status is published later (the last bytes must land first), and
    /// in between the pid may already be reusable: signalling on it would hit
    /// whatever now owns that pid.
    reaped: Arc<std::sync::atomic::AtomicBool>,
}

impl std::fmt::Debug for ProcessSession {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("sandbox.ProcessSession")
            .field("id", &self.id)
            .field("pid", &self.pid)
            .finish_non_exhaustive()
    }
}

impl ProcessSession {
    fn lock_closed(&self) -> Result<MutexGuard<'_, bool>> {
        self.closed.lock().map_err(|_| poisoned())
    }

    fn is_closed(&self) -> bool {
        self.lock_closed().map(|closed| *closed).unwrap_or(true)
    }

    fn is_reaped(&self) -> bool {
        self.reaped.load(std::sync::atomic::Ordering::Acquire)
    }

    /// Wait for the reaper to publish the outcome.
    async fn await_exit(&self) -> Option<ExitStatus> {
        let mut exited = self.exited.clone();
        loop {
            if let Some(exit) = exited.borrow().clone() {
                return Some(exit);
            }
            if exited.changed().await.is_err() {
                return exited.borrow().clone();
            }
        }
    }

    /// Stop the process group: `SIGTERM`, a grace period, then `SIGKILL`.
    async fn stop(&self, reason: ExitReason) -> Result<()> {
        if self.exit().is_some() {
            return Ok(());
        }
        {
            let mut pending = self.pending.lock().map_err(|_| poisoned())?;
            if pending.is_none() {
                *pending = Some(reason);
            }
        }
        let Some(pid) = self.pid else {
            return Ok(());
        };
        if self.is_reaped() {
            // The pid may already belong to someone else; the exit is on its way.
            return Ok(());
        }
        signal_group(pid, GroupSignal::Terminate);
        if tokio::time::timeout(TERMINATE_GRACE, self.await_exit())
            .await
            .is_ok()
        {
            return Ok(());
        }
        if !self.is_reaped() {
            signal_group(pid, GroupSignal::Kill);
        }
        // Bounded: a child that survived SIGKILL, or a descendant that keeps the
        // output pipes open, must not make `terminate`/`close` hang forever.
        let _ = tokio::time::timeout(TERMINATE_GRACE, self.await_exit()).await;
        #[cfg(not(unix))]
        {
            // Without a signal mechanism there is nothing further to do; the
            // Windows lane replaces this with a job object (see the plan, W1).
            return Err(Error::not_available(
                "sandbox: terminate needs a job object on this platform (not implemented yet)",
            ));
        }
        #[cfg(unix)]
        Ok(())
    }
}

/// Which signal to deliver to a process group.
enum GroupSignal {
    Interrupt,
    Terminate,
    Kill,
}

#[async_trait]
impl Session for ProcessSession {
    fn id(&self) -> &str {
        &self.id
    }

    fn pid(&self) -> Option<u32> {
        self.pid
    }

    fn features(&self) -> SessionFeatures {
        self.features
    }

    fn exit(&self) -> Option<ExitStatus> {
        self.exited.borrow().clone()
    }

    async fn read(&self, after_seq: u64, max_bytes: usize) -> Result<ReadResult> {
        // The log answers `Closed` on its own, which keeps a read that is
        // already blocked consistent with one that starts afterwards.
        self.log.read(after_seq, max_bytes).await
    }

    async fn write(&self, data: &[u8]) -> Result<()> {
        if self.is_closed() {
            return Err(closed_error());
        }
        let mut guard = self.stdin.lock().await;
        let stdin = guard.as_mut().ok_or_else(|| {
            Error::not_available("sandbox: session was started without stdin; set spec.stdin")
        })?;
        stdin.write_all(data).await.map_err(Error::from)?;
        stdin.flush().await.map_err(Error::from)?;
        Ok(())
    }

    async fn close_input(&self) -> Result<()> {
        // A session that never declared stdin has nothing to close: the caller
        // that never asked for an input side needs no special case.
        let mut guard = self.stdin.lock().await;
        if let Some(mut stdin) = guard.take() {
            let _ = stdin.shutdown().await;
        }
        Ok(())
    }

    async fn resize(&self, _rows: u16, _cols: u16) -> Result<()> {
        if self.is_closed() {
            return Err(closed_error());
        }
        Err(Error::not_available(
            "sandbox: resize requires a pty session",
        ))
    }

    async fn signal(&self, signal: Signal) -> Result<()> {
        if self.is_closed() {
            return Err(closed_error());
        }
        if self.exit().is_some() || self.is_reaped() {
            return Err(closed_error());
        }
        let Some(pid) = self.pid else {
            return Ok(());
        };
        match signal {
            Signal::Interrupt => signal_group(pid, GroupSignal::Interrupt),
        }
        Ok(())
    }

    async fn terminate(&self) -> Result<()> {
        self.stop(ExitReason::Terminated).await
    }

    async fn wait(&self) -> Result<ExitStatus> {
        if let Some(exit) = self.exit() {
            return Ok(exit);
        }
        self.await_exit().await.ok_or_else(closed_error)
    }

    fn watch(&self) -> Result<EventStream> {
        self.log.watch()
    }

    async fn close(&self) -> Result<()> {
        {
            let mut closed = self.lock_closed()?;
            if *closed {
                return Ok(());
            }
            *closed = true;
        }
        // The supervisor is *not* aborted: it owns the armed control group and
        // releases it when it observes the exit. Aborting it here would leak the
        // group (and its accounting would then poison a re-used session id).
        let stopped = self.stop(ExitReason::Terminated).await;
        self.log.close();
        stopped
    }
}

/// Classify how the child ended.
fn classify(status: &std::process::ExitStatus, pending: Option<ExitReason>) -> ExitStatus {
    #[cfg(unix)]
    let signal = {
        use std::os::unix::process::ExitStatusExt;
        status.signal()
    };
    #[cfg(not(unix))]
    let signal: Option<i32> = None;

    let code = status.code();
    let reason = match pending {
        Some(reason) => reason,
        None if signal.is_some() => ExitReason::Signaled,
        None => ExitReason::Exited,
    };
    ExitStatus {
        code,
        signal,
        reason,
    }
}

/// Validate the policy this backend can actually enforce, and resolve the
/// working directory against the runner root when there is one.
fn validate_spec(spec: &mut SessionSpec, options: &SpawnOptions) -> Result<()> {
    if spec.argv.is_empty() {
        return Err(Error::validation("sandbox: argv is empty"));
    }
    spec.opts.net.validate()?;
    if spec.opts.net.mode != crate::contract::NetMode::Default {
        return Err(Error::not_available(format!(
            "sandbox: the local runner cannot enforce net mode {}; use a backend with filesystem bounds",
            spec.opts.net.mode.as_str()
        )));
    }
    if !spec.opts.net.unix_sockets.is_empty() {
        return Err(Error::not_available(
            "sandbox: the local runner cannot confine unix socket egress",
        ));
    }
    if spec.opts.write != crate::contract::WriteMode::Workspace {
        return Err(Error::not_available(
            "sandbox: the local runner has no filesystem boundary, so a read-only root cannot be enforced",
        ));
    }
    // Resource caps: the platform decides what it can enforce, and a request it
    // cannot honour is refused rather than silently ignored.
    options.limits.check(&spec.opts.limits, spec.opts.timeout)?;
    match &options.root {
        Some(root) => {
            // Containment is checked twice (lexically, then through symlinks) by
            // the shared resolver, so `local`, `bwrap` and `seatbelt` cannot
            // disagree about what "inside the root" means.
            let resolved =
                crate::confinement::path::resolve_workdir(root, spec.opts.workdir.as_deref())?;
            spec.opts.workdir = Some(resolved);
        }
        None => {
            if let Some(dir) = &spec.opts.workdir
                && !dir.is_dir()
            {
                return Err(Error::not_found(format!(
                    "sandbox: workdir {} does not exist",
                    dir.display()
                )));
            }
        }
    }
    Ok(())
}

/// Apply [`EnvPolicy`] to a command.
fn apply_env(command: &mut Command, policy: &EnvPolicy) {
    match &policy.allow {
        // `None` keeps the inherited environment: the historical behaviour a
        // deployment that never configured an allow-list relies on.
        None => {}
        Some(names) => {
            command.env_clear();
            for name in names {
                if let Ok(value) = std::env::var(name) {
                    command.env(name, value);
                }
            }
        }
    }
    for (name, value) in &policy.inject {
        command.env(name, value);
    }
}

/// Spawn one pipe-backed session.
pub(crate) async fn spawn(
    mut spec: SessionSpec,
    options: &SpawnOptions,
) -> Result<Arc<dyn Session>> {
    validate_spec(&mut spec, options)?;
    // A pty is a different session shape (merged stream, resize, no stdin EOF),
    // so it has its own implementation rather than a flag here.
    if spec.tty {
        let limits = &spec.opts.limits;
        if limits.memory_bytes.is_some() || limits.cpu_millicores.is_some() {
            // The control group is armed on the pipe path; a pty session does not
            // go through it yet, and running unguarded would break the promise.
            return Err(Error::not_available(
                "sandbox: a pty session cannot enforce memory/cpu limits yet; \
                 start it without limits or use a pipe session",
            ));
        }
        return crate::session::terminal::spawn(spec, options).await;
    }

    let mut command = Command::new(&spec.argv[0]);
    command.args(&spec.argv[1..]);
    let wants_stdin = spec.wants_stdin();
    command.stdin(if wants_stdin {
        Stdio::piped()
    } else {
        // A program that branches on fd 0 must see `/dev/null`, not an empty
        // pipe that never ends.
        Stdio::null()
    });
    command.stdout(Stdio::piped());
    command.stderr(Stdio::piped());
    apply_env(&mut command, &spec.opts.env);
    if let Some(dir) = &spec.opts.workdir {
        command.current_dir(dir);
    }
    #[cfg(unix)]
    command.process_group(0);

    // Prepare before spawning so a refusal leaves no process behind; attach after,
    // because a control group needs the pid.
    let plan = options
        .limits
        .plan(&spec.id, &spec.opts.limits, spec.opts.timeout)?;

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
    let armed = match plan
        .map(|plan| plan.arm(child.id().unwrap_or_default()))
        .transpose()
    {
        Ok(armed) => armed,
        Err(error) => {
            // The child exists but cannot be accounted for: kill it rather than
            // run unguarded.
            let _ = child.start_kill();
            let _ = child.wait().await;
            return Err(error);
        }
    };
    let stdout = child.stdout.take();
    let stderr = child.stderr.take();
    let stdin = child.stdin.take();
    let limit = spec
        .opts
        .limits
        .max_output_bytes
        .filter(|limit| *limit > 0)
        .unwrap_or(options.default_output_limit);
    let log = Arc::new(Log::new(limit));
    let pending: Arc<Mutex<Option<ExitReason>>> = Arc::new(Mutex::new(None));
    let reaped: Arc<std::sync::atomic::AtomicBool> =
        Arc::new(std::sync::atomic::AtomicBool::new(false));
    let (exited_tx, exited) = watch::channel(None);

    // One owner of the child: the reaper waits, joins both pumps, and only then
    // marks the log finished, so the last chunk is never lost to a race.
    let mut pumps = Vec::new();
    if let Some(reader) = stdout {
        pumps.push(tokio::spawn(pump(log.clone(), reader, Stream::Stdout)));
    }
    if let Some(reader) = stderr {
        pumps.push(tokio::spawn(pump(log.clone(), reader, Stream::Stderr)));
    }
    {
        let log = log.clone();
        let pending = pending.clone();
        let reaped = reaped.clone();
        let mut child = child;
        tokio::spawn(async move {
            let status = child.wait().await;
            // The pid is now reusable: no caller may signal it again.
            reaped.store(true, std::sync::atomic::Ordering::Release);
            // Dropping the child also drops the stdin pipe, which signals EOF
            // to a child that is still reading it.
            drop(child);
            // Bounded: a descendant that escaped the process group keeps fd 1/2
            // open, so the pumps would otherwise wait for it forever.
            let joins = async {
                for handle in pumps {
                    let _ = handle.await;
                }
            };
            let _ = tokio::time::timeout(TERMINATE_GRACE, joins).await;
            let reason = pending.lock().ok().and_then(|slot| *slot);
            let exit = match status {
                Ok(status) => classify(&status, reason),
                Err(error) => {
                    tracing::warn!(%error, "sandbox: reaping the session failed");
                    ExitStatus {
                        code: None,
                        signal: None,
                        reason: reason.unwrap_or(ExitReason::Exited),
                    }
                }
            };
            log.finish(exit.clone());
            exited_tx.send_replace(Some(exit));
        });
    }

    let session = Arc::new(ProcessSession {
        id: spec.id.clone(),
        pid,
        features: SessionFeatures {
            tty: false,
            stdin: wants_stdin,
            signal: cfg!(unix),
            events: true,
        },
        log: log.clone(),
        stdin: AsyncMutex::new(stdin),
        exited,
        pending: pending.clone(),
        closed: Mutex::new(false),
        reaped: reaped.clone(),
    });

    // `Opts.stdin` is one-shot `Exec` input, not a declaration: `start` neither
    // delivers those bytes nor lets them open an input side. A caller that wants
    // a streaming session calls `write`/`close_input` itself, and `exec` does
    // exactly that.
    if !wants_stdin {
        session.close_input().await?;
    }
    if spec.opts.timeout.is_some() || armed.is_some() {
        // Detached on purpose: the task ends by itself once the exit is
        // published (it observes `exited`), and it is the only owner of `armed`,
        // so nothing else may cancel it mid-release.
        supervise(session.clone(), spec.opts.timeout, armed);
    }

    Ok(session)
}

/// Watch one session's deadline and resource budgets.
///
/// One task for both, because they answer the same question ("should this stop,
/// and why") and a session that is killed twice must keep the first reason:
/// [`ProcessSession::stop`] only records a reason the first time.
fn supervise(session: Arc<ProcessSession>, timeout: Option<Duration>, mut armed: Option<Armed>) {
    tokio::spawn(async move {
        let deadline = timeout.map(|timeout| tokio::time::Instant::now() + timeout);
        let mut exited = session.exited.clone();
        loop {
            // The process is gone: stop accounting, and let the reaper publish.
            if exited.borrow().is_some() {
                if let Some(armed) = armed.take() {
                    armed.close();
                }
                return;
            }

            if let Some(limits) = &mut armed {
                match limits.check() {
                    // A cap fired: the process group goes with the whole tree.
                    Ok(Some(reason)) => {
                        let _ = session.stop(reason).await;
                        if let Some(armed) = armed.take() {
                            armed.close();
                        }
                        return;
                    }
                    Ok(None) => {}
                    // Accounting is gone: kill it rather than run it unguarded.
                    Err(error) => {
                        tracing::debug!(%error, "sandbox: resource accounting was lost");
                        let _ = session.stop(ExitReason::Unenforceable).await;
                        if let Some(armed) = armed.take() {
                            armed.close();
                        }
                        return;
                    }
                }
            }

            if let Some(deadline) = deadline
                && tokio::time::Instant::now() >= deadline
            {
                let _ = session.stop(ExitReason::TimedOut).await;
                if let Some(armed) = armed.take() {
                    armed.close();
                }
                return;
            }

            tokio::select! {
                _ = tokio::time::sleep(SUPERVISE_TICK) => {}
                _ = exited.changed() => {}
            }
        }
    });
}

/// Read one stream to EOF into the log.
async fn pump(log: Arc<Log>, mut reader: impl AsyncRead + Unpin, stream: Stream) {
    let mut buffer = [0u8; PUMP_BUFFER];
    loop {
        match reader.read(&mut buffer).await {
            Ok(0) => break,
            Ok(read) => log.push(stream, &buffer[..read]),
            Err(error) => {
                tracing::debug!(%error, stream = stream.as_str(), "sandbox: output pump ended");
                break;
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn spec(argv: &[&str]) -> SessionSpec {
        SessionSpec::new(argv.iter().map(|arg| (*arg).to_owned()).collect())
    }

    fn options() -> SpawnOptions {
        SpawnOptions {
            root: None,
            default_output_limit: DEFAULT_OUTPUT_LIMIT,
            limits: std::sync::Arc::new(Manager::probe()),
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
            if read.eof {
                return out;
            }
        }
    }

    #[tokio::test]
    async fn runs_a_command_and_reports_output_and_exit() {
        let session = spawn(spec(&["sh", "-c", "echo hello; exit 3"]), &options())
            .await
            .unwrap();
        let exit = session.wait().await.unwrap();
        assert_eq!(exit.code, Some(3));
        assert_eq!(exit.reason, ExitReason::Exited);
        assert!(read_all(&session).await.contains("hello"));
        session.close().await.unwrap();
    }

    #[tokio::test]
    async fn stderr_is_a_separate_stream() {
        let session = spawn(spec(&["sh", "-c", "echo out; echo err 1>&2"]), &options())
            .await
            .unwrap();
        session.wait().await.unwrap();
        let mut after = 0;
        let mut stdout = String::new();
        let mut stderr = String::new();
        loop {
            let read = session.read(after, 64 * 1024).await.unwrap();
            for chunk in &read.chunks {
                let text = String::from_utf8_lossy(&chunk.data).into_owned();
                match chunk.stream {
                    Stream::Stdout | Stream::Tty => stdout.push_str(&text),
                    Stream::Stderr => stderr.push_str(&text),
                }
            }
            after = read.next_seq;
            if read.eof {
                break;
            }
        }
        assert_eq!(stdout, "out\n");
        assert_eq!(stderr, "err\n");
        session.close().await.unwrap();
    }

    #[tokio::test]
    async fn stdin_is_delivered_and_can_be_closed() {
        let mut start = spec(&["cat"]);
        start.stdin = true;
        let session = spawn(start, &options()).await.unwrap();
        session.write(b"ping\n").await.unwrap();
        session.close_input().await.unwrap();
        let exit = session.wait().await.unwrap();
        assert_eq!(exit.code, Some(0));
        assert!(read_all(&session).await.contains("ping"));
        session.close().await.unwrap();
    }

    #[tokio::test]
    async fn close_input_without_stdin_is_a_no_op() {
        let session = spawn(spec(&["true"]), &options()).await.unwrap();
        session.close_input().await.unwrap();
        session.wait().await.unwrap();
        session.close().await.unwrap();
    }

    #[tokio::test]
    async fn writing_without_an_input_side_is_not_available() {
        let session = spawn(spec(&["sleep", "5"]), &options()).await.unwrap();
        let error = session.write(b"x").await.expect_err("no stdin");
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::NotAvailable);
        session.close().await.unwrap();
    }

    #[tokio::test]
    async fn the_environment_is_an_allow_list() {
        let mut start = spec(&["sh", "-c", "printf '%s' \"${SECRET:-missing}\""]);
        start
            .opts
            .env
            .inject
            .insert("SECRET".to_owned(), "s3cr3t".to_owned());
        let session = spawn(start, &options()).await.unwrap();
        session.wait().await.unwrap();
        assert_eq!(read_all(&session).await, "s3cr3t");
        session.close().await.unwrap();
    }

    #[tokio::test]
    async fn an_empty_allow_list_inherits_nothing() {
        // Nothing is forwarded, so even a variable the daemon certainly has
        // (`HOME`) must be absent in the child.
        let mut start = spec(&["sh", "-c", "printf '%s' \"${HOME:-missing}\""]);
        start.opts.env.allow = Some(Vec::new());
        let session = spawn(start, &options()).await.unwrap();
        session.wait().await.unwrap();
        assert_eq!(read_all(&session).await, "missing");
        session.close().await.unwrap();
    }

    #[tokio::test]
    async fn an_allow_list_forwards_only_the_named_variables() {
        // The list names a variable that does not exist, which proves the
        // filter runs: `HOME` would have been inherited if it did not.
        let mut start = spec(&["sh", "-c", "printf '%s' \"${HOME:-missing}\""]);
        start.opts.env.allow = Some(vec!["FLOWCRAFT_TEST_ABSENT".to_owned()]);
        let session = spawn(start, &options()).await.unwrap();
        session.wait().await.unwrap();
        assert_eq!(read_all(&session).await, "missing");
        session.close().await.unwrap();
    }

    #[tokio::test]
    async fn a_none_allow_list_inherits_the_host_environment() {
        let expected = std::env::var("HOME").unwrap_or_else(|_| "missing".to_owned());
        let start = spec(&["sh", "-c", "printf '%s' \"${HOME:-missing}\""]);
        let session = spawn(start, &options()).await.unwrap();
        session.wait().await.unwrap();
        assert_eq!(read_all(&session).await, expected);
        session.close().await.unwrap();
    }

    #[tokio::test]
    async fn timeouts_are_classified_not_merely_killed() {
        let mut start = spec(&["sleep", "30"]);
        start.opts.timeout = Some(Duration::from_millis(100));
        let session = spawn(start, &options()).await.unwrap();
        let exit = session.wait().await.unwrap();
        assert_eq!(exit.reason, ExitReason::TimedOut);
        assert_eq!(
            exit.error().unwrap().kind(),
            flowcraft_error::ErrorKind::Timeout
        );
        session.close().await.unwrap();
    }

    #[tokio::test]
    async fn terminate_is_a_graceful_signal_first() {
        let session = spawn(
            spec(&[
                "sh",
                "-c",
                "trap 'exit 7' TERM; while :; do sleep 0.05; done",
            ]),
            &options(),
        )
        .await
        .unwrap();
        // Let the shell install the trap: `spawn` returns at exec, not after the
        // script's first statement.
        tokio::time::sleep(Duration::from_millis(150)).await;
        let started = std::time::Instant::now();
        session.terminate().await.unwrap();
        let exit = session.wait().await.unwrap();
        assert_eq!(exit.reason, ExitReason::Terminated);
        assert_eq!(exit.code, Some(7), "the trap ran, so SIGTERM landed first");
        assert!(started.elapsed() < TERMINATE_GRACE, "no escalation needed");
        session.close().await.unwrap();
    }

    #[tokio::test]
    async fn terminate_escalates_when_the_child_ignores_sigterm() {
        let session = spawn(
            spec(&["sh", "-c", "trap '' TERM; while :; do sleep 0.05; done"]),
            &options(),
        )
        .await
        .unwrap();
        // Without this the default SIGTERM disposition is still in force and
        // the escalation below would never be exercised.
        tokio::time::sleep(Duration::from_millis(150)).await;
        let started = std::time::Instant::now();
        session.terminate().await.unwrap();
        let exit = session.wait().await.unwrap();
        assert_eq!(exit.reason, ExitReason::Terminated);
        assert!(
            started.elapsed() >= TERMINATE_GRACE,
            "expected the grace period before SIGKILL, took {:?}",
            started.elapsed()
        );
        assert_eq!(exit.signal, Some(9));
        session.close().await.unwrap();
    }

    #[tokio::test]
    async fn a_signaled_exit_is_classified() {
        let session = spawn(spec(&["sh", "-c", "kill -TERM $$"]), &options())
            .await
            .unwrap();
        let exit = session.wait().await.unwrap();
        assert_eq!(exit.reason, ExitReason::Signaled);
        assert_eq!(exit.signal, Some(15));
        session.close().await.unwrap();
    }

    #[tokio::test]
    async fn closing_releases_every_later_call() {
        let session = spawn(spec(&["sleep", "30"]), &options()).await.unwrap();
        session.close().await.unwrap();
        for error in [
            session.read(0, 16).await.err().unwrap(),
            session.write(b"x").await.err().unwrap(),
            session.signal(Signal::Interrupt).await.err().unwrap(),
            session.resize(10, 10).await.err().unwrap(),
        ] {
            assert_eq!(error.kind(), flowcraft_error::ErrorKind::Closed, "{error}");
        }
        // Close is idempotent.
        session.close().await.unwrap();
    }

    #[tokio::test]
    async fn unsupported_policies_are_refused() {
        let mut net = spec(&["true"]);
        net.opts.net.mode = crate::contract::NetMode::DenyAll;
        assert_eq!(
            spawn(net, &options()).await.err().unwrap().kind(),
            flowcraft_error::ErrorKind::NotAvailable
        );

        let mut write = spec(&["true"]);
        write.opts.write = crate::contract::WriteMode::ReadOnly;
        assert_eq!(
            spawn(write, &options()).await.err().unwrap().kind(),
            flowcraft_error::ErrorKind::NotAvailable
        );

        let mut memory = spec(&["true"]);
        memory.opts.limits.memory_bytes = Some(1024);
        assert_eq!(
            spawn(memory, &options()).await.err().unwrap().kind(),
            flowcraft_error::ErrorKind::NotAvailable
        );

        // A pty is no longer an unsupported policy: it has its own session shape
        // and its own tests.
    }

    #[tokio::test]
    async fn a_missing_command_is_not_found() {
        let error = spawn(spec(&["flowcraft-command-that-does-not-exist"]), &options())
            .await
            .expect_err("not found");
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::NotFound);
    }

    #[tokio::test]
    async fn output_is_bounded_and_reports_a_gap() {
        let mut start = spec(&["sh", "-c", "yes x | head -c 200000"]);
        start.opts.limits.max_output_bytes = Some(64);
        let session = spawn(start, &options()).await.unwrap();
        session.wait().await.unwrap();
        // The retained window is tiny, so an early cursor is a gap rather than
        // a silent hole.
        let error = session.read(0, 1024).await.expect_err("gap");
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::Aborted);
        session.close().await.unwrap();
    }
}
