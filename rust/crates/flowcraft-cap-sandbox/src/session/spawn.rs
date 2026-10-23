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
use crate::session::log::Log;

/// Output retained when the spec declares no bound (10 MiB, as the Go side).
pub(crate) const DEFAULT_OUTPUT_LIMIT: u64 = 10 * 1024 * 1024;

/// How long a `SIGTERM` gets before the group is killed outright.
const TERMINATE_GRACE: Duration = Duration::from_secs(2);

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
    watchdog: Mutex<Option<tokio::task::JoinHandle<()>>>,
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
        signal_group(pid, GroupSignal::Terminate);
        if tokio::time::timeout(TERMINATE_GRACE, self.await_exit())
            .await
            .is_ok()
        {
            return Ok(());
        }
        signal_group(pid, GroupSignal::Kill);
        let _ = self.await_exit().await;
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
        if self.exit().is_some() {
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
        if let Ok(mut watchdog) = self.watchdog.lock()
            && let Some(handle) = watchdog.take()
        {
            handle.abort();
        }
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

/// Validate the policy this backend can actually enforce.
fn validate_spec(spec: &SessionSpec) -> Result<()> {
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
    let limits = &spec.opts.limits;
    if limits.cpu_millicores.is_some() || limits.memory_bytes.is_some() {
        if limits.cpu_millicores.is_some() && spec.opts.timeout.is_none() {
            return Err(Error::not_available(
                "sandbox: cpu_millicores needs a per-call timeout to derive a cpu-time budget",
            ));
        }
        return Err(Error::not_available(
            "sandbox: this backend cannot enforce cpu/memory limits",
        ));
    }
    if limits.disk_bytes.is_some() {
        return Err(Error::not_available(
            "sandbox: disk limits are not supported (no quota mechanism)",
        ));
    }
    if spec.tty {
        return Err(Error::not_available(
            "sandbox: this build has no pty; start the session with tty: false",
        ));
    }
    if let Some(dir) = &spec.opts.workdir
        && !dir.is_dir()
    {
        return Err(Error::not_found(format!(
            "sandbox: workdir {} does not exist",
            dir.display()
        )));
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
    spec: SessionSpec,
    default_output_limit: u64,
) -> Result<Arc<dyn Session>> {
    validate_spec(&spec)?;

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
    let limit = spec
        .opts
        .limits
        .max_output_bytes
        .filter(|limit| *limit > 0)
        .unwrap_or(default_output_limit);
    let log = Arc::new(Log::new(limit));
    let pending: Arc<Mutex<Option<ExitReason>>> = Arc::new(Mutex::new(None));
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
        let mut child = child;
        tokio::spawn(async move {
            let status = child.wait().await;
            // Dropping the child also drops the stdin pipe, which signals EOF
            // to a child that is still reading it.
            drop(child);
            for handle in pumps {
                let _ = handle.await;
            }
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
        watchdog: Mutex::new(None),
    });

    if let Some(bytes) = &spec.opts.stdin
        && !bytes.is_empty()
    {
        let mut guard = session.stdin.lock().await;
        if let Some(stdin) = guard.as_mut() {
            stdin.write_all(bytes).await.map_err(Error::from)?;
            stdin.flush().await.map_err(Error::from)?;
        }
    }
    if !wants_stdin {
        session.close_input().await?;
    }
    if let Some(timeout) = spec.opts.timeout {
        let watchdog_session = session.clone();
        let handle = tokio::spawn(async move {
            tokio::time::sleep(timeout).await;
            if watchdog_session.exit().is_none() {
                let _ = watchdog_session.stop(ExitReason::TimedOut).await;
            }
        });
        if let Ok(mut slot) = session.watchdog.lock() {
            *slot = Some(handle);
        }
    }

    Ok(session)
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
        let session = spawn(
            spec(&["sh", "-c", "echo hello; exit 3"]),
            DEFAULT_OUTPUT_LIMIT,
        )
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
        let session = spawn(
            spec(&["sh", "-c", "echo out; echo err 1>&2"]),
            DEFAULT_OUTPUT_LIMIT,
        )
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
        let session = spawn(start, DEFAULT_OUTPUT_LIMIT).await.unwrap();
        session.write(b"ping\n").await.unwrap();
        session.close_input().await.unwrap();
        let exit = session.wait().await.unwrap();
        assert_eq!(exit.code, Some(0));
        assert!(read_all(&session).await.contains("ping"));
        session.close().await.unwrap();
    }

    #[tokio::test]
    async fn close_input_without_stdin_is_a_no_op() {
        let session = spawn(spec(&["true"]), DEFAULT_OUTPUT_LIMIT).await.unwrap();
        session.close_input().await.unwrap();
        session.wait().await.unwrap();
        session.close().await.unwrap();
    }

    #[tokio::test]
    async fn writing_without_an_input_side_is_not_available() {
        let session = spawn(spec(&["sleep", "5"]), DEFAULT_OUTPUT_LIMIT)
            .await
            .unwrap();
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
        let session = spawn(start, DEFAULT_OUTPUT_LIMIT).await.unwrap();
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
        let session = spawn(start, DEFAULT_OUTPUT_LIMIT).await.unwrap();
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
        let session = spawn(start, DEFAULT_OUTPUT_LIMIT).await.unwrap();
        session.wait().await.unwrap();
        assert_eq!(read_all(&session).await, "missing");
        session.close().await.unwrap();
    }

    #[tokio::test]
    async fn a_none_allow_list_inherits_the_host_environment() {
        let expected = std::env::var("HOME").unwrap_or_else(|_| "missing".to_owned());
        let start = spec(&["sh", "-c", "printf '%s' \"${HOME:-missing}\""]);
        let session = spawn(start, DEFAULT_OUTPUT_LIMIT).await.unwrap();
        session.wait().await.unwrap();
        assert_eq!(read_all(&session).await, expected);
        session.close().await.unwrap();
    }

    #[tokio::test]
    async fn timeouts_are_classified_not_merely_killed() {
        let mut start = spec(&["sleep", "30"]);
        start.opts.timeout = Some(Duration::from_millis(100));
        let session = spawn(start, DEFAULT_OUTPUT_LIMIT).await.unwrap();
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
            DEFAULT_OUTPUT_LIMIT,
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
            DEFAULT_OUTPUT_LIMIT,
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
        let session = spawn(spec(&["sh", "-c", "kill -TERM $$"]), DEFAULT_OUTPUT_LIMIT)
            .await
            .unwrap();
        let exit = session.wait().await.unwrap();
        assert_eq!(exit.reason, ExitReason::Signaled);
        assert_eq!(exit.signal, Some(15));
        session.close().await.unwrap();
    }

    #[tokio::test]
    async fn closing_releases_every_later_call() {
        let session = spawn(spec(&["sleep", "30"]), DEFAULT_OUTPUT_LIMIT)
            .await
            .unwrap();
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
            spawn(net, DEFAULT_OUTPUT_LIMIT).await.err().unwrap().kind(),
            flowcraft_error::ErrorKind::NotAvailable
        );

        let mut write = spec(&["true"]);
        write.opts.write = crate::contract::WriteMode::ReadOnly;
        assert_eq!(
            spawn(write, DEFAULT_OUTPUT_LIMIT)
                .await
                .err()
                .unwrap()
                .kind(),
            flowcraft_error::ErrorKind::NotAvailable
        );

        let mut memory = spec(&["true"]);
        memory.opts.limits.memory_bytes = Some(1024);
        assert_eq!(
            spawn(memory, DEFAULT_OUTPUT_LIMIT)
                .await
                .err()
                .unwrap()
                .kind(),
            flowcraft_error::ErrorKind::NotAvailable
        );

        let mut tty = spec(&["true"]);
        tty.tty = true;
        assert_eq!(
            spawn(tty, DEFAULT_OUTPUT_LIMIT).await.err().unwrap().kind(),
            flowcraft_error::ErrorKind::NotAvailable
        );
    }

    #[tokio::test]
    async fn a_missing_command_is_not_found() {
        let error = spawn(
            spec(&["flowcraft-command-that-does-not-exist"]),
            DEFAULT_OUTPUT_LIMIT,
        )
        .await
        .expect_err("not found");
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::NotFound);
    }

    #[tokio::test]
    async fn output_is_bounded_and_reports_a_gap() {
        let mut start = spec(&["sh", "-c", "yes x | head -c 200000"]);
        start.opts.limits.max_output_bytes = Some(64);
        let session = spawn(start, DEFAULT_OUTPUT_LIMIT).await.unwrap();
        session.wait().await.unwrap();
        // The retained window is tiny, so an early cursor is a gap rather than
        // a silent hole.
        let error = session.read(0, 1024).await.expect_err("gap");
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::Aborted);
        session.close().await.unwrap();
    }
}
