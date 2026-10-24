//! The one-shot view of a session.
//!
//! [`exec`] is a *derived* helper, not a second execution path: it starts an
//! ordinary non-tty session, drives it to completion, and closes it. Every
//! policy check (env, net, write, limits, workdir, and — for a confined backend
//! — the wrapper itself) therefore applies exactly as it does for `start`.
//!
//! Two behaviours are worth stating because they surprise callers:
//!
//! - a non-zero exit is **not** an error: the code is the answer. Only a
//!   session-level failure (a timeout, an exhausted budget, a refused policy)
//!   is an `Err`.
//! - output is captured *per stream* and each stream is truncated on its own,
//!   unlike the session ring which is one byte budget. `Exec` is a summary, and
//!   a chatty stderr must not eat stdout.

use flowcraft_error::Result;

use crate::contract::{Chunk, ExitStatus, Runner, Session, SessionSpec, Stream};

/// The default per-stream capture bound (10 MiB), as the Go side.
pub(crate) const DEFAULT_CAPTURE_LIMIT: u64 = 10 * 1024 * 1024;

/// How many bytes one read asks for.
const READ_BATCH: usize = 64 * 1024;

/// The outcome of a one-shot call.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ExecResult {
    /// Exit code; `-1` when the process was killed by a signal.
    pub exit_code: i32,
    /// Captured standard output, truncated to the limit.
    pub stdout: String,
    /// Captured standard error, truncated to the limit.
    pub stderr: String,
}

impl ExecResult {
    /// Whether the process exited with status 0.
    #[must_use]
    pub fn success(&self) -> bool {
        self.exit_code == 0
    }
}

/// Run `spec` to completion and capture its output.
///
/// The session is always released, including on the error path, so a caller
/// cannot leak a process group *or* a session-table entry by returning early —
/// a one-shot call leaves no trace in [`Runner::list`].
///
/// Output beyond the bound is truncated per stream and dropped silently, which is
/// the one-shot contract on the Go side as well: `Exec` is a summary, and a chatty
/// stderr must not eat stdout. A caller that needs to know the difference should
/// use a session and read with a cursor.
pub async fn exec(runner: &dyn Runner, mut spec: SessionSpec) -> Result<ExecResult> {
    // `Exec` is the one-shot form: a tty would merge the streams and make the
    // capture meaningless.
    spec.tty = false;
    let stdin = spec.opts.stdin.take().filter(|bytes| !bytes.is_empty());
    // The bytes declare an input side; `start` never delivers them on its own,
    // because streaming input belongs to `Session::write`.
    if stdin.is_some() {
        spec.stdin = true;
    }

    let limit = spec
        .opts
        .limits
        .max_output_bytes
        .filter(|limit| *limit > 0)
        .unwrap_or(DEFAULT_CAPTURE_LIMIT);
    // The ring must not be squeezed to the *result* bound: with two pumps racing,
    // bytes would be trimmed before this reader sees them and `read` would report a
    // sequence gap instead of a truncated result. The session keeps its own
    // default; truncation happens at capture time, below.
    spec.opts.limits.max_output_bytes = None;

    let session = runner.start(spec).await?;
    let id = session.id().to_owned();
    let outcome = async {
        if let Some(bytes) = stdin {
            session.write(&bytes).await?;
        }
        if session.features().stdin {
            session.close_input().await?;
        }
        run(&*session, limit).await
    }
    .await;
    // Release the session even when the run failed; a failed *release* must not
    // mask the original error, so it is only reported when nothing else failed.
    let released = runner.close_session(&id).await;
    let (stdout, stderr, exit) = outcome?;
    match released {
        Ok(()) => {}
        // The session may already be gone (a second close, a dead endpoint).
        Err(error)
            if matches!(
                error.kind(),
                flowcraft_error::ErrorKind::NotFound | flowcraft_error::ErrorKind::Closed
            ) => {}
        Err(error) => return Err(error),
    }

    if let Some(error) = exit.error() {
        return Err(error);
    }
    Ok(ExecResult {
        exit_code: exit.code.unwrap_or(-1),
        stdout,
        stderr,
    })
}

type Captured = (String, String, ExitStatus);

async fn run(session: &dyn Session, limit: u64) -> Result<Captured> {
    let mut stdout = Vec::new();
    let mut stderr = Vec::new();
    let mut cursor = 0;
    loop {
        let read = session.read(cursor, READ_BATCH).await?;
        for chunk in &read.chunks {
            capture(chunk, &mut stdout, &mut stderr, limit);
        }
        cursor = read.next_seq;
        if read.eof {
            break;
        }
    }
    let exit = session.wait().await?;
    Ok((
        String::from_utf8_lossy(&stdout).into_owned(),
        String::from_utf8_lossy(&stderr).into_owned(),
        exit,
    ))
}

/// Append one chunk to its stream, respecting that stream's own bound.
fn capture(chunk: &Chunk, stdout: &mut Vec<u8>, stderr: &mut Vec<u8>, limit: u64) {
    let target = match chunk.stream {
        // A merged pty stream is not produced here (`exec` forces `tty: false`),
        // but treating it as stdout keeps this total.
        Stream::Stdout | Stream::Tty => stdout,
        Stream::Stderr => stderr,
    };
    let room = usize::try_from(limit)
        .unwrap_or(usize::MAX)
        .saturating_sub(target.len());
    let take = room.min(chunk.data.len());
    target.extend_from_slice(&chunk.data[..take]);
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::contract::{ExitReason, NetMode, SessionSpec, WriteMode};
    use crate::local::LocalRunner;
    use std::time::Duration;

    fn spec(argv: &[&str]) -> SessionSpec {
        SessionSpec::new(argv.iter().map(|arg| (*arg).to_owned()).collect())
    }

    #[tokio::test]
    async fn captures_stdout_and_a_zero_exit() {
        let runner = LocalRunner::new();
        let result = exec(&runner, spec(&["sh", "-c", "echo hello"]))
            .await
            .unwrap();
        assert_eq!(result.exit_code, 0);
        assert!(result.success());
        assert_eq!(result.stdout, "hello\n");
        assert_eq!(result.stderr, "");
    }

    #[tokio::test]
    async fn a_non_zero_exit_is_the_answer_not_an_error() {
        let runner = LocalRunner::new();
        let result = exec(&runner, spec(&["sh", "-c", "exit 3"])).await.unwrap();
        assert_eq!(result.exit_code, 3);
        assert!(!result.success());
    }

    #[tokio::test]
    async fn stderr_is_captured_separately() {
        let runner = LocalRunner::new();
        let result = exec(&runner, spec(&["sh", "-c", "echo out; echo err 1>&2"]))
            .await
            .unwrap();
        assert_eq!(result.stdout, "out\n");
        assert_eq!(result.stderr, "err\n");
    }

    #[tokio::test]
    async fn stdin_bytes_are_delivered_and_the_pipe_closed() {
        let runner = LocalRunner::new();
        let mut request = spec(&["cat"]);
        request.opts.stdin = Some(b"ping\n".to_vec());
        let result = exec(&runner, request).await.unwrap();
        assert_eq!(result.exit_code, 0);
        assert_eq!(result.stdout, "ping\n");
    }

    #[tokio::test]
    async fn without_stdin_bytes_the_child_reads_dev_null() {
        let runner = LocalRunner::new();
        // `cat` on a pipe would never see EOF; on /dev/null it returns at once.
        let result = exec(&runner, spec(&["cat"])).await.unwrap();
        assert_eq!(result.exit_code, 0);
        assert_eq!(result.stdout, "");
    }

    #[tokio::test]
    async fn the_capture_bound_is_not_handed_to_the_session_ring() {
        // A small result bound with output far larger than it must return a
        // truncated result, never an `Aborted` sequence gap: the ring keeps its own
        // budget and trimming happens at capture.
        let runner = LocalRunner::new();
        let mut request = spec(&[
            "sh",
            "-c",
            "head -c 65536 /dev/zero | tr '\\0' a; head -c 65536 /dev/zero | tr '\\0' b 1>&2",
        ]);
        request.opts.limits.max_output_bytes = Some(64);
        let result = exec(&runner, request).await.expect("truncated, not a gap");
        assert_eq!(result.stdout.len(), 64);
        assert_eq!(result.stderr.len(), 64);
    }

    #[tokio::test]
    async fn each_stream_is_truncated_on_its_own() {
        let runner = LocalRunner::new();
        let mut request = spec(&[
            "sh",
            "-c",
            "head -c 4096 /dev/zero | tr '\\0' a; head -c 4096 /dev/zero | tr '\\0' b 1>&2",
        ]);
        request.opts.limits.max_output_bytes = Some(64);
        let result = exec(&runner, request).await.unwrap();
        assert_eq!(result.stdout.len(), 64, "stdout has its own budget");
        assert_eq!(result.stderr.len(), 64, "stderr is not starved by stdout");
        assert!(result.stdout.starts_with('a'));
        assert!(result.stderr.starts_with('b'));
    }

    #[tokio::test]
    async fn a_timeout_is_an_error_and_still_closes_the_session() {
        let runner = LocalRunner::new();
        let mut request = spec(&["sleep", "30"]);
        request.opts.timeout = Some(Duration::from_millis(100));
        let error = exec(&runner, request).await.expect_err("timeout");
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::Timeout);
        // The session was closed: nothing is left in the runner.
        assert!(runner.list().await.unwrap().is_empty());
    }

    #[tokio::test]
    async fn a_signaled_process_reports_minus_one() {
        let runner = LocalRunner::new();
        let result = exec(&runner, spec(&["sh", "-c", "kill -TERM $$"]))
            .await
            .unwrap();
        assert_eq!(result.exit_code, -1);
    }

    #[tokio::test]
    async fn a_refused_policy_is_an_error() {
        let runner = LocalRunner::new();
        let mut request = spec(&["true"]);
        request.opts.net.mode = NetMode::DenyAll;
        let error = exec(&runner, request).await.expect_err("unsupported");
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::NotAvailable);

        let mut request = spec(&["true"]);
        request.opts.write = WriteMode::ReadOnly;
        assert_eq!(
            exec(&runner, request)
                .await
                .expect_err("unsupported")
                .kind(),
            flowcraft_error::ErrorKind::NotAvailable
        );
    }

    #[tokio::test]
    async fn a_confined_backend_is_driven_through_the_same_helper() {
        use crate::confinement::{ConfinedRunner, Sandbox, SandboxConfig};

        if !std::path::Path::new("/usr/bin/sandbox-exec").exists() {
            return;
        }
        let dir = tempfile::tempdir().expect("tempdir");
        let runner = ConfinedRunner::new(SandboxConfig::new(Sandbox::Seatbelt, dir.path()));
        let result = exec(&runner, spec(&["sh", "-c", "echo confined"]))
            .await
            .unwrap();
        assert_eq!(result.exit_code, 0);
        assert_eq!(result.stdout, "confined\n");
    }

    #[tokio::test]
    async fn an_exit_reason_is_never_confused_with_a_plain_exit() {
        let status = ExitStatus::exited(0);
        assert_eq!(status.reason, ExitReason::Exited);
        assert!(status.error().is_none());
    }
}
