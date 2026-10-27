//! The Windows pty: a ConPTY session, stopped through its job object.
//!
//! Why this cannot reuse the pipe path: a terminal is a *different session shape*
//! — one merged stream, resize, and no stdin EOF. What it shares with the pipe
//! path is everything the caller sees (`Log`, cursors, `ExitStatus`, the record of
//! why it ended), so both produce the same `Session` and differ only in how bytes
//! travel and how the tree is stopped.
//!
//! The pty itself comes from [`flowcraft_win32::Pty`], not `portable-pty`, because
//! the confinement phases need to own `CreateProcess`: the token (W3) and the app
//! container (W4) are attached at creation time, and this phase needs
//! `CREATE_NEW_PROCESS_GROUP` so a console event can address the tree.
//!
//! Ctrl-C is a byte, not a call: `GenerateConsoleCtrlEvent` only reaches a group
//! attached to the *caller's* console, and the child's console is a pseudo one.
//! Writing `0x03` is what a keyboard does, and the console host turns it into
//! `CTRL_C_EVENT` for the foreground group.

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};

use async_trait::async_trait;
use tokio::io::AsyncWriteExt;
use tokio::sync::watch;

use crate::caps::sandbox::codes::{self, Code};
use crate::caps::sandbox::contract::{
    EventStream, ExitReason, ExitStatus, ReadResult, Session, SessionFeatures, SessionSpec, Signal,
    Stream, WriteMode,
};
use crate::caps::sandbox::session::job::SessionJob;
use crate::caps::sandbox::session::log::Log;
use crate::caps::sandbox::session::spawn::{SpawnOptions, TERMINATE_GRACE};
use crate::error::{Error, Result};

/// How many bytes one read takes from the console.
const READ_CHUNK: usize = 8192;

/// How often a capped session asks its job whether the budget is spent. The same
/// interval the pipe path's supervisor uses.
const BUDGET_TICK: std::time::Duration = std::time::Duration::from_millis(50);

/// Whether this build can provide a pty.
pub(crate) fn available() -> bool {
    flowcraft_win32::Pty::available()
}

/// One pty-backed session.
pub(crate) struct PtySession {
    id: String,
    pid: Option<u32>,
    features: SessionFeatures,
    log: Arc<Log>,
    /// Held so `resize` can reach the console, and so `close` can release it in
    /// the only safe order: after the child is gone and the pump has drained.
    pty: Mutex<Option<flowcraft_win32::Pty>>,
    /// Shared with the pump, which answers a console's cursor requests through it.
    writer: std::sync::Arc<tokio::sync::Mutex<Option<tokio::fs::File>>>,
    /// The tree boundary: the only way to stop a pty session on Windows.
    job: Option<SessionJob>,
    pending: Arc<Mutex<Option<ExitReason>>>,
    exited: watch::Receiver<Option<ExitStatus>>,
    closed: AtomicBool,
    /// The timeout task, aborted on close so a long deadline does not pin the
    /// console after the session is gone.
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

    /// Stop the tree.
    ///
    /// No grace period: `TerminateJobObject` is synchronous, and Windows has no
    /// `SIGTERM` for a tree to catch. The pending reason wins, so a caller that
    /// asked for `TimedOut` keeps it.
    async fn stop(&self, reason: ExitReason) -> Result<()> {
        if self.exited.borrow().is_some() {
            return Ok(());
        }
        if let Ok(mut pending) = self.pending.lock()
            && pending.is_none()
        {
            *pending = Some(reason);
        }
        let Some(job) = &self.job else {
            return Err(Error::not_available(
                "sandbox: this pty session has no job object to stop",
            ));
        };
        job.terminate()?;
        let _ = tokio::time::timeout(TERMINATE_GRACE, self.await_exit()).await;
        Ok(())
    }
}

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
        let mut writer = self.writer.lock().await;
        let Some(writer) = writer.as_mut() else {
            return Err(Error::not_available(
                "sandbox: the session's terminal is closed",
            ));
        };
        writer.write_all(data).await.map_err(Error::from)?;
        writer.flush().await.map_err(Error::from)
    }

    async fn close_input(&self) -> Result<()> {
        // A terminal has no EOF; closing it would mean sending EOT, which is a
        // control character the child may or may not act on.
        Err(Error::not_available(
            "sandbox: a pty session has no stdin to close; write Ctrl-D instead",
        ))
    }

    async fn resize(&self, rows: u16, cols: u16) -> Result<()> {
        if self.is_closed() {
            return Err(Self::closed_error());
        }
        let (default_rows, default_cols) = (24u16, 80u16);
        let pty = self
            .pty
            .lock()
            .map_err(|_| Error::internal("sandbox: pty console lock poisoned"))?;
        let Some(pty) = pty.as_ref() else {
            return Err(Error::not_available("sandbox: the terminal is closed"));
        };
        pty.resize(
            if cols == 0 { default_cols } else { cols },
            if rows == 0 { default_rows } else { rows },
        )
        .map_err(|error| Error::not_available(format!("sandbox: resize the pty: {error}")))?;
        Ok(())
    }

    async fn signal(&self, signal: Signal) -> Result<()> {
        if self.is_closed() {
            return Err(Self::closed_error());
        }
        if self.exited.borrow().is_some() {
            return Err(Self::closed_error());
        }
        match signal {
            // Authentic Ctrl-C, exactly as the unix path does it: the driver
            // signals the foreground group, and a child in raw mode ignores it.
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
        // Order matters and it is the one ConPTY is strict about:
        //   1. end the tree, so the child releases the console;
        //   2. let the pump see EOF, which the reaper's bounded join covers;
        //   3. only then release the console, because `ClosePseudoConsole` waits
        //      for the console host to drain and would block on a live reader.
        let stopped = self.stop(ExitReason::Terminated).await;
        let _ = tokio::time::timeout(TERMINATE_GRACE, self.await_exit()).await;
        if let Ok(mut writer) = self.writer.try_lock() {
            writer.take();
        }
        // `ClosePseudoConsole` blocks until the console host drains, so it is not
        // a call to make on a runtime worker: the drop happens on a thread that is
        // allowed to block.
        if let Some(pty) = self.pty.lock().ok().and_then(|mut slot| slot.take()) {
            let _ = tokio::task::spawn_blocking(move || drop(pty)).await;
        }
        self.log.close();
        stopped
    }
}

/// Spawn a pty session.
pub(crate) async fn spawn(spec: SessionSpec, options: &SpawnOptions) -> Result<Arc<dyn Session>> {
    if !available() {
        return Err(codes::unavailable(
            Code::TtyUnsupported,
            "sandbox: this host has no ConPTY; Windows 10 1809 or later is required",
        ));
    }
    let (rows, cols) = spec.window();
    // The caps must exist before the child does, and the job is also what stops
    // the session: a pty without one could not be terminated at all.
    let job = SessionJob::create(&spec.opts.limits, spec.opts.timeout)?;
    let mut pty = flowcraft_win32::Pty::open(cols, rows)
        .map_err(|error| Error::not_available(format!("sandbox: open a pty: {error}")))?;
    let input = pty.take_input();
    let output = pty.take_output();

    let env = super::spawn::environment(&spec.opts.env);
    // The write boundary: a read-only call gets a Low-integrity token, which the
    // kernel enforces as "no write up" — the same mechanism the pipe path uses.
    let token = match spec.opts.write {
        WriteMode::ReadOnly => Some(
            flowcraft_win32::Token::derive(flowcraft_win32::Integrity::Low)
                .map_err(|error| Error::not_available(format!("sandbox: {error}")))?,
        ),
        WriteMode::Workspace => None,
    };
    let child = pty
        .spawn(flowcraft_win32::Launch {
            command: flowcraft_win32::Command {
                program: &spec.argv[0],
                args: &spec.argv[1..],
                cwd: spec.opts.workdir.as_deref(),
                env: env.as_ref(),
            },
            job: Some(job.job()),
            token: token.as_ref(),
        })
        .map_err(|error| {
            Error::not_available(format!(
                "sandbox: cannot start {:?} on a pty: {error}",
                spec.argv[0]
            ))
        })?;
    let pid = child.pid();

    let limit = spec
        .opts
        .limits
        .max_output_bytes
        .filter(|limit| *limit > 0)
        .unwrap_or(options.default_output_limit);
    let log = Arc::new(Log::new(limit));
    let pending: Arc<Mutex<Option<ExitReason>>> = Arc::new(Mutex::new(None));
    let (exited_tx, exited) = watch::channel(None);
    // The input side is shared: the session streams stdin through it, and the pump
    // answers the console's cursor requests through the same handle.
    let writer: Arc<tokio::sync::Mutex<Option<tokio::fs::File>>> = Arc::new(
        tokio::sync::Mutex::new(input.map(tokio::fs::File::from_std)),
    );

    // The console's output is one stream, and that is what a tty is: a single
    // `Stream::Tty` carrying both what the child wrote to stdout and to stderr.
    let pump = match output {
        Some(output) => {
            let log = log.clone();
            let responder = writer.clone();
            let mut reader = tokio::fs::File::from_std(output);
            Some(tokio::spawn(async move {
                // A console asks its terminal where the cursor is and waits for the
                // answer; a pump that only reads leaves the application stalled.
                let mut cursor = flowcraft_win32::CursorReport::new();
                let mut buffer = [0u8; READ_CHUNK];
                loop {
                    match tokio::io::AsyncReadExt::read(&mut reader, &mut buffer).await {
                        Ok(0) => break,
                        Ok(read) => {
                            if let Some(answer) = cursor.feed(&buffer[..read]) {
                                let mut writer = responder.lock().await;
                                if let Some(writer) = writer.as_mut() {
                                    let _ = writer.write_all(answer).await;
                                    let _ = writer.flush().await;
                                }
                            }
                            log.push(Stream::Tty, &buffer[..read]);
                        }
                        Err(error) if error.kind() == std::io::ErrorKind::Interrupted => continue,
                        Err(error) => {
                            tracing::debug!(%error, "sandbox: pty pump ended");
                            break;
                        }
                    }
                }
            }))
        }
        None => None,
    };

    // The waiter blocks on the process handle, so it gets its own thread; the
    // pump is joined (bounded) before the log is finished, so the last bytes are
    // never lost to a race with `wait`.
    let reap_log = log.clone();
    let reap_pending = pending.clone();
    let reap_job = job.clone();
    tokio::spawn(async move {
        let status = tokio::task::spawn_blocking(move || child.wait()).await;
        if let Some(pump) = pump {
            let _ = tokio::time::timeout(TERMINATE_GRACE, pump).await;
        }
        // A stop the caller asked for keeps its reason; otherwise the job's
        // counters decide between "exited" and "over budget".
        let reason = reap_pending
            .lock()
            .ok()
            .and_then(|slot| *slot)
            .or_else(|| reap_job.violation());
        let exit = match status {
            Ok(Ok(code)) => ExitStatus {
                code: Some(code.cast_signed()),
                signal: None,
                reason: reason.unwrap_or(ExitReason::Exited),
            },
            Ok(Err(error)) => {
                tracing::warn!(%error, "sandbox: reaping the pty session failed");
                ExitStatus {
                    code: None,
                    signal: None,
                    reason: reason.unwrap_or(ExitReason::Exited),
                }
            }
            Err(error) => {
                tracing::warn!(%error, "sandbox: the pty waiter task failed");
                ExitStatus {
                    code: None,
                    signal: None,
                    reason: reason.unwrap_or(ExitReason::Exited),
                }
            }
        };
        reap_log.finish(exit.clone());
        exited_tx.send_replace(Some(exit));
    });

    let session = Arc::new(PtySession {
        id: spec.id.clone(),
        pid: Some(pid),
        features: SessionFeatures {
            tty: true,
            stdin: true,
            signal: true,
            events: true,
        },
        log,
        pty: Mutex::new(Some(pty)),
        writer,
        job: Some(job),
        pending,
        exited,
        closed: AtomicBool::new(false),
        watchdog: Mutex::new(None),
    });

    // A pty session has no `supervise` loop (the caps are job settings, not an
    // armed group), so the same "has the budget been spent" question is asked by a
    // watcher of its own: a memory ceiling denies allocations rather than killing
    // the tree, and a program that never gives up has to be ended here.
    if spec.opts.limits.memory_bytes.is_some() || spec.opts.limits.cpu_millicores.is_some() {
        let budget = session.clone();
        tokio::spawn(async move {
            loop {
                if budget.exit().is_some() {
                    return;
                }
                if let Some(reason) = budget.job.as_ref().and_then(|job| job.violation()) {
                    let _ = budget.stop(reason).await;
                    return;
                }
                tokio::time::sleep(BUDGET_TICK).await;
            }
        });
    }

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
    Ok(session)
}

// The environment policy is shared with the pipe path (`session::spawn`): both
// spawns build the same block from the same policy, so a session cannot see a
// different environment depending on whether it got a terminal.

#[cfg(test)]
mod tests {
    use super::*;
    use crate::caps::sandbox::contract::EnvPolicy;
    use crate::caps::sandbox::session::spawn::environment;

    fn spec(argv: &[&str]) -> SessionSpec {
        SessionSpec::new(argv.iter().map(|arg| (*arg).to_owned()).collect())
    }

    #[test]
    fn a_pty_is_available_on_this_host() {
        assert!(available(), "a Windows runner has ConPTY");
    }

    #[test]
    fn an_allow_list_does_not_inherit_the_rest() {
        let policy = EnvPolicy {
            allow: Some(Vec::new()),
            inject: [("ONLY".to_owned(), "1".to_owned())].into_iter().collect(),
        };
        let env = environment(&policy).expect("an explicit block");
        assert_eq!(env.len(), 1);
        assert_eq!(env.get("ONLY").map(String::as_str), Some("1"));
    }

    #[test]
    fn an_unrestricted_policy_inherits() {
        assert!(environment(&EnvPolicy::default()).is_none());
    }

    #[test]
    fn a_spec_carries_the_window_it_asked_for() {
        let mut spec = spec(&["cmd"]);
        assert_eq!(spec.window(), (24, 80));
        spec.rows = 40;
        spec.cols = 120;
        assert_eq!(spec.window(), (40, 120));
    }
}
