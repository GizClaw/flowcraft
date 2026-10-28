//! The pty session.
//!
//! A pty is a genuinely different session shape, not a flag on the pipe one:
//!
//! - stdout and stderr are **merged** into one `tty` stream;
//! - the window can be **resized**, and Ctrl-C is a byte (`0x03`) written to the
//!   terminal rather than a signal — which also means a child that puts the
//!   terminal in raw mode legitimately ignores it;
//! - there is **no way to close stdin**: a terminal has no EOF, and Go's contract
//!   answers `NotAvailable` for the same reason;
//! - `portable-pty`'s API is blocking, so the reader pump, the writer and the
//!   reap each run on a blocking thread (`spawn_blocking`). That is the price of
//!   not writing `setsid`/`TIOCSCTTY` ourselves, which this workspace's
//!   `unsafe_code = "forbid"` makes impossible.
//!
//! Everything a caller relies on is shared with the pipe path: the same [`Log`]
//! (so cursors, gaps and `watch` behave identically), the same SIGTERM → grace →
//! SIGKILL stop sequence, and the same exit classification.

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};

use crate::error::{Error, Result};
use async_trait::async_trait;
use portable_pty::{ChildKiller, CommandBuilder, MasterPty, PtySize, native_pty_system};
use tokio::sync::watch;

use crate::caps::sandbox::contract::{
    EventStream, ExitReason, ExitStatus, ReadResult, Session, SessionFeatures, SessionSpec, Signal,
    Stream,
};
use crate::caps::sandbox::session::log::Log;
use crate::caps::sandbox::session::registry::Started;
use crate::caps::sandbox::session::spawn::{DEFAULT_OUTPUT_LIMIT, SpawnOptions, TERMINATE_GRACE};

/// Whether this build can provide a pty.
pub(crate) const fn available() -> bool {
    true
}

/// One pty-backed session.
pub(crate) struct PtySession {
    id: String,
    pid: Option<u32>,
    features: SessionFeatures,
    log: Arc<Log>,
    /// `MasterPty` is `Send` but not `Sync`, and a session must be both: the lock
    /// is the smallest way to satisfy the contract.
    master: Mutex<Box<dyn MasterPty + Send>>,
    writer: Arc<Mutex<Option<Box<dyn std::io::Write + Send>>>>,
    killer: Mutex<Box<dyn ChildKiller + Send + Sync>>,
    pending: Arc<Mutex<Option<ExitReason>>>,
    exited: watch::Receiver<Option<ExitStatus>>,
    closed: AtomicBool,
    /// The timeout task, aborted on close so a long timeout does not pin the
    /// session (and its master fd) after it is gone.
    watchdog: Mutex<Option<tokio::task::JoinHandle<()>>>,
}

impl std::fmt::Debug for PtySession {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("sandbox.PtySession")
            .field("id", &self.id)
            .field("pid", &self.pid)
            .finish_non_exhaustive()
    }
}

impl PtySession {
    fn is_closed(&self) -> bool {
        self.closed.load(Ordering::Acquire)
    }

    fn closed_error() -> Error {
        Error::closed("sandbox: session is closed")
    }

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

    /// Deliver a group signal, falling back to the child killer.
    fn signal(&self, signal: GroupSignal) {
        if self.pid.is_some() {
            signal_group(self.pid, signal);
            return;
        }
        if let Ok(mut killer) = self.killer.lock() {
            let _ = killer.kill();
        }
    }

    /// `SIGTERM` to the process group, then `SIGKILL` after the grace period.
    async fn stop(&self, reason: ExitReason) -> Result<()> {
        if self.exited.borrow().is_some() {
            return Ok(());
        }
        if let Ok(mut pending) = self.pending.lock()
            && pending.is_none()
        {
            *pending = Some(reason);
        }
        // The pty child is its own session leader, so the whole group can be
        // signalled; `killer` is the fallback when the platform does not report a
        // pid.
        self.signal(GroupSignal::Terminate);
        if tokio::time::timeout(TERMINATE_GRACE, self.await_exit())
            .await
            .is_ok()
        {
            return Ok(());
        }
        self.signal(GroupSignal::Kill);
        let _ = tokio::time::timeout(TERMINATE_GRACE, self.await_exit()).await;
        Ok(())
    }
}

/// Which signal to deliver to the session's process group.
enum GroupSignal {
    Terminate,
    Kill,
}

#[cfg(unix)]
fn signal_group(pid: Option<u32>, signal: GroupSignal) {
    use nix::sys::signal::{Signal as Unix, killpg};
    use nix::unistd::Pid;

    let unix = match signal {
        GroupSignal::Terminate => Unix::SIGTERM,
        GroupSignal::Kill => Unix::SIGKILL,
    };
    if let Some(pid) = pid
        && let Ok(raw) = i32::try_from(pid)
    {
        let _ = killpg(Pid::from_raw(raw), unix);
    }
}

#[cfg(not(unix))]
fn signal_group(_pid: Option<u32>, _signal: GroupSignal) {}

#[async_trait]
impl Session for PtySession {
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
        self.log.read(after_seq, max_bytes).await
    }

    async fn write(&self, data: &[u8]) -> Result<()> {
        if self.is_closed() {
            return Err(Self::closed_error());
        }
        let writer = self.writer.clone();
        let data = data.to_vec();
        tokio::task::spawn_blocking(move || {
            let Ok(mut guard) = writer.lock() else {
                return Err(Error::internal("sandbox: pty writer lock poisoned"));
            };
            let Some(writer) = guard.as_mut() else {
                return Err(Error::not_available(
                    "sandbox: the session's terminal is closed",
                ));
            };
            use std::io::Write;
            writer.write_all(&data).map_err(Error::from)?;
            writer.flush().map_err(Error::from)
        })
        .await
        .map_err(|error| Error::internal(format!("sandbox: pty write task: {error}")))?
    }

    async fn close_input(&self) -> Result<()> {
        // A terminal has no EOF; closing it would mean sending EOT, which is a
        // control character the child may or may not act on. Go refuses for the
        // same reason, so a caller gets one consistent answer.
        Err(Error::not_available(
            "sandbox: a pty session has no stdin to close; write Ctrl-D instead",
        ))
    }

    async fn resize(&self, rows: u16, cols: u16) -> Result<()> {
        if self.is_closed() {
            return Err(Self::closed_error());
        }
        let (default_rows, default_cols) = (24u16, 80u16);
        let master = self
            .master
            .lock()
            .map_err(|_| Error::internal("sandbox: pty master lock poisoned"))?;
        master
            .resize(PtySize {
                rows: if rows == 0 { default_rows } else { rows },
                cols: if cols == 0 { default_cols } else { cols },
                pixel_width: 0,
                pixel_height: 0,
            })
            .map_err(|error| Error::from(std::io::Error::other(error.to_string())))
    }

    async fn signal(&self, signal: Signal) -> Result<()> {
        if self.is_closed() {
            return Err(Self::closed_error());
        }
        if self.exited.borrow().is_some() {
            return Err(Self::closed_error());
        }
        match signal {
            // Authentic Ctrl-C: the terminal driver signals the foreground
            // process group. A child in raw mode ignores it, which is correct.
            Signal::Interrupt => self.write(&[0x03]).await,
        }
    }

    async fn terminate(&self) -> Result<()> {
        self.stop(ExitReason::Terminated).await
    }

    async fn wait(&self) -> Result<ExitStatus> {
        if let Some(exit) = self.exit() {
            return Ok(exit);
        }
        self.await_exit().await.ok_or_else(Self::closed_error)
    }

    fn watch(&self) -> Result<EventStream> {
        self.log.watch()
    }

    async fn close(&self) -> Result<()> {
        if self.closed.swap(true, Ordering::AcqRel) {
            return Ok(());
        }
        if let Ok(mut watchdog) = self.watchdog.lock()
            && let Some(handle) = watchdog.take()
        {
            handle.abort();
        }
        // Stop the child *first*: a blocking `write` holds the writer mutex while
        // it waits for queue space, so taking the lock before the kill would park
        // a runtime worker forever. Killing the child closes the slave side, the
        // pending write returns, and the writer becomes free.
        let stopped = self.stop(ExitReason::Terminated).await;
        if let Ok(mut guard) = self.writer.try_lock() {
            guard.take();
        }
        // A writer still held by its own blocking task is dropped there when the
        // fd errors out; nothing here may wait for it.
        self.log.close();
        stopped
    }
}

/// Spawn a pty session.
pub(crate) async fn spawn(spec: SessionSpec, options: &SpawnOptions) -> Result<Started> {
    let (rows, cols) = spec.window();
    let pty = native_pty_system();
    let pair = pty
        .openpty(PtySize {
            rows,
            cols,
            pixel_width: 0,
            pixel_height: 0,
        })
        .map_err(|error| Error::not_available(format!("sandbox: open a pty: {error}")))?;

    let mut command = CommandBuilder::new(&spec.argv[0]);
    command.args(&spec.argv[1..]);
    apply_env(&mut command, &spec.opts.env);
    if let Some(dir) = &spec.opts.workdir {
        command.cwd(dir);
    }

    let mut child = pair
        .slave
        .spawn_command(command)
        .map_err(|error| Error::not_available(format!("sandbox: start a pty session: {error}")))?;
    // The slave is only needed until the child owns it.
    drop(pair.slave);

    let pid = child.process_id();
    let killer = child.clone_killer();
    let reader = pair
        .master
        .try_clone_reader()
        .map_err(|error| Error::internal(format!("sandbox: pty reader: {error}")))?;
    let writer = pair
        .master
        .take_writer()
        .map_err(|error| Error::internal(format!("sandbox: pty writer: {error}")))?;

    let limit = spec
        .opts
        .limits
        .max_output_bytes
        .filter(|limit| *limit > 0)
        .unwrap_or(options.default_output_limit);
    let log = Arc::new(Log::new(limit));
    let pending: Arc<Mutex<Option<ExitReason>>> = Arc::new(Mutex::new(None));
    let (exited_tx, exited) = watch::channel(None);

    // The reader is blocking, so it gets its own thread; the merged stream is
    // what makes a tty a single `Stream::Tty`.
    let pump = {
        let log = log.clone();
        tokio::task::spawn_blocking(move || {
            let mut reader = reader;
            let mut buffer = [0u8; 8192];
            loop {
                match std::io::Read::read(&mut reader, &mut buffer) {
                    Ok(0) => break,
                    Ok(read) => log.push(Stream::Tty, &buffer[..read]),
                    Err(error) if error.kind() == std::io::ErrorKind::Interrupted => continue,
                    Err(error) => {
                        tracing::debug!(%error, "sandbox: pty pump ended");
                        break;
                    }
                }
            }
        })
    };

    // One blocking thread owns the child and joins the pump before finishing the
    // log, so the last bytes are never lost to a race with `wait`.
    let reap_log = log.clone();
    let reap_pending = pending.clone();
    tokio::spawn(async move {
        let status = tokio::task::spawn_blocking(move || child.wait()).await;
        // Bounded for the same reason as the pipe path: an escaped descendant can
        // keep the master open.
        let _ = tokio::time::timeout(TERMINATE_GRACE, pump).await;
        let reason = reap_pending.lock().ok().and_then(|slot| *slot);
        let exit = match status {
            Ok(Ok(status)) => classify(&status, reason),
            _ => ExitStatus {
                code: None,
                signal: None,
                reason: reason.unwrap_or(ExitReason::Exited),
            },
        };
        reap_log.finish(exit.clone());
        exited_tx.send_replace(Some(exit));
    });

    let session = Arc::new(PtySession {
        id: spec.id.clone(),
        pid,
        features: SessionFeatures {
            tty: true,
            stdin: true,
            signal: true,
            events: true,
        },
        log: log.clone(),
        master: Mutex::new(pair.master),
        writer: Arc::new(Mutex::new(Some(writer))),
        killer: Mutex::new(killer),
        pending: pending.clone(),
        exited,
        closed: AtomicBool::new(false),
        watchdog: Mutex::new(None),
    });

    if let Some(timeout) = spec.opts.timeout {
        let watchdog = session.clone();
        let handle = tokio::spawn(async move {
            tokio::time::sleep(timeout).await;
            if watchdog.exit().is_none() {
                let _ = watchdog.stop(ExitReason::TimedOut).await;
            }
        });
        if let Ok(mut slot) = session.watchdog.lock() {
            *slot = Some(handle);
        }
    }
    Ok(Started { session, log })
}

/// Classify a pty exit.
///
/// `portable-pty` exposes the terminating signal by name, not by number, so a tty
/// session reports `Signaled` without the number; the pipe path keeps the number
/// because `std::process` gives it.
fn classify(status: &portable_pty::ExitStatus, pending: Option<ExitReason>) -> ExitStatus {
    let signalled = status.signal().is_some();
    let reason = match pending {
        Some(reason) => reason,
        None if signalled => ExitReason::Signaled,
        None => ExitReason::Exited,
    };
    ExitStatus {
        code: if signalled {
            None
        } else {
            i32::try_from(status.exit_code()).ok()
        },
        signal: None,
        reason,
    }
}

/// Apply the same environment policy the pipe path uses.
fn apply_env(command: &mut CommandBuilder, policy: &crate::caps::sandbox::contract::EnvPolicy) {
    match &policy.allow {
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

/// Keep the default limit referenced even when no session declares one.
const _: u64 = DEFAULT_OUTPUT_LIMIT;

#[cfg(test)]
mod tests {
    use super::*;
    use crate::caps::sandbox::contract::{ExitReason, Runner, SessionSpec};
    use crate::caps::sandbox::local::LocalRunner;
    use std::time::Duration;

    fn spec(argv: &[&str]) -> SessionSpec {
        let mut spec = SessionSpec::new(argv.iter().map(|arg| (*arg).to_owned()).collect());
        spec.tty = true;
        spec
    }

    async fn read_all(session: &Arc<dyn Session>) -> String {
        let mut out = String::new();
        let mut after = 0;
        loop {
            let read = session.read(after, 64 * 1024).await.expect("read");
            for chunk in &read.chunks {
                assert_eq!(chunk.stream, Stream::Tty, "a pty merges the streams");
                out.push_str(&String::from_utf8_lossy(&chunk.data));
            }
            after = read.next_seq;
            if read.eof {
                return out;
            }
        }
    }

    #[tokio::test]
    async fn a_pty_session_merges_the_streams_and_reports_an_exit() {
        let runner = LocalRunner::new();
        let session = runner
            .start(spec(&["sh", "-c", "echo out; echo err 1>&2"]))
            .await
            .expect("start");
        let exit = session.wait().await.expect("wait");
        assert_eq!(exit.code, Some(0));
        let text = read_all(&session).await;
        assert!(text.contains("out"), "{text:?}");
        assert!(text.contains("err"), "{text:?}");
        runner.close().await.expect("close");
    }

    #[tokio::test]
    async fn a_pty_session_declares_what_it_provides() {
        let runner = LocalRunner::new();
        let session = runner
            .start(spec(&["sh", "-c", "true"]))
            .await
            .expect("start");
        let features = session.features();
        assert!(features.tty);
        assert!(features.stdin);
        assert!(features.signal);
        assert!(features.events);
        session.wait().await.expect("wait");
        runner.close().await.expect("close");
    }

    #[tokio::test]
    async fn the_window_can_be_resized() {
        let runner = LocalRunner::new();
        let mut request = spec(&["sh", "-c", "sleep 5"]);
        request.rows = 40;
        request.cols = 120;
        let session = runner.start(request).await.expect("start");
        session.resize(50, 132).await.expect("resize");
        session.terminate().await.expect("terminate");
        session.wait().await.expect("wait");
        runner.close().await.expect("close");
    }

    #[tokio::test]
    async fn ctrl_c_is_a_byte_on_the_terminal() {
        let runner = LocalRunner::new();
        let session = runner
            .start(spec(&[
                "sh",
                "-c",
                "trap 'exit 0' INT; while :; do sleep 0.05; done",
            ]))
            .await
            .expect("start");
        tokio::time::sleep(Duration::from_millis(200)).await;
        session.signal(Signal::Interrupt).await.expect("signal");
        let exit = session.wait().await.expect("wait");
        assert_eq!(
            exit.code,
            Some(0),
            "the trap ran on the interrupt: {exit:?}"
        );
        runner.close().await.expect("close");
    }

    #[tokio::test]
    async fn stdin_can_be_written_and_has_no_close() {
        let runner = LocalRunner::new();
        let session = runner.start(spec(&["cat"])).await.expect("start");
        session.write(b"ping\n").await.expect("write");
        assert_eq!(
            session
                .close_input()
                .await
                .expect_err("a terminal has no EOF")
                .kind(),
            crate::error::ErrorKind::NotAvailable
        );
        session.terminate().await.expect("terminate");
        runner.close().await.expect("close");
    }

    #[tokio::test]
    async fn a_timeout_stops_a_pty_session() {
        let runner = LocalRunner::new();
        let mut request = spec(&["sleep", "30"]);
        request.opts.timeout = Some(Duration::from_millis(120));
        let session = runner.start(request).await.expect("start");
        let exit = session.wait().await.expect("wait");
        assert_eq!(exit.reason, ExitReason::TimedOut);
        runner.close().await.expect("close");
    }

    #[tokio::test]
    async fn closing_releases_a_pty_session() {
        let runner = LocalRunner::new();
        let session = runner.start(spec(&["sleep", "30"])).await.expect("start");
        session.close().await.expect("close");
        assert_eq!(
            session.read(0, 8).await.expect_err("closed").kind(),
            crate::error::ErrorKind::Closed
        );
        session.close().await.expect("idempotent");
        runner.close().await.expect("close");
    }

    #[tokio::test]
    async fn the_runner_claims_a_pty_only_because_it_can_provide_one() {
        let capabilities = LocalRunner::new().capabilities().await.expect("caps");
        assert_eq!(capabilities.features.tty, available());
    }
}
