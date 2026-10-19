//! `core/hooks/run.go`: one command hook, run to completion or killed.
//!
//! Everything here is the transport contract's execution half: `sh -c` in the
//! source's directory, the event object on stdin, the timeout with a process
//! group kill behind it, the bounded capture, and the one rule that shapes all
//! of it -- failure never propagates. A warning goes to the sink and the next
//! hook runs.
//!
//! # What the port changed
//!
//! Core merges stdout and stderr into one `io.Writer` before it starts the
//! command, so its capture holds the two streams in arrival order. A Rust
//! `Command` cannot point two pipes at one writer, so the port gives each stream
//! a reader task that appends to one capture under a lock: the same buffer, the
//! same 64 KiB cap, the same truncation marker, and -- because the two readers
//! race exactly as two copying goroutines do -- the same arrival order in
//! practice. Nothing in the contract compares the interleaving, because nothing
//! could.
//!
//! The wait for the output pipe to close is bounded on both sides, but the port
//! says so differently: core sets `Cmd.WaitDelay` and gets `exec.ErrWaitDelay`,
//! the port bounds the join of its two readers. The warning a caller sees is the
//! same sentence either way.

use std::sync::{Arc, Mutex};
use std::time::Duration;

use tokio::io::AsyncWriteExt;

use crate::{DEFAULT_TIMEOUT, GroupEntry, Hook, Payload, event_body};

/// Interprets every hook command.
pub const SHELL: &str = "sh";

/// How much combined output a hook run retains for its log line. A hook that
/// writes more than this is not blocked: the excess is counted as truncated and
/// dropped.
pub const MAX_OUTPUT: usize = 64 << 10;

/// Bounds the wait for the output pipe to close once the command (or its process
/// group) is gone, so a hook that leaves a background process holding the pipe
/// cannot stall the run.
pub const WAIT_DELAY: Duration = Duration::from_secs(1);

/// The content-bearing payload fields an untrusted source never sees: they carry
/// conversation text, tool arguments, tool results or a subagent's instructions,
/// none of which a plugin command needs in order to observe that an event
/// happened.
pub const STRIPPED: [&str; 7] = [
    "tool_input",
    "tool_result",
    "prompt",
    "command",
    "error",
    "message",
    "target",
];

/// A hook source that could not be read or parsed was skipped.
pub const SKIPPING_SOURCE: &str = "hooks: skipping hook source";
/// A payload that would not encode: a producer handed the runner something JSON
/// cannot carry.
pub const ENCODE_FAILED: &str = "hooks: encode event payload failed";
/// A hook that outlived its timeout. It, and its process group, were killed.
pub const DID_NOT_FINISH: &str = "hooks: command hook did not finish in time";
/// A hook that exited, but left a descendant holding its output pipe.
pub const LEFT_PIPE_OPEN: &str = "hooks: command hook left its output pipe open";
/// A hook that ran and failed, or could not start at all.
pub const COMMAND_FAILED: &str = "hooks: command hook failed";

/// Every warning the runner emits, in the order the module documents them.
/// [`crate::Manager::load_with_sources`] and [`crate::Manager::fire`] between
/// them emit exactly these.
pub const WARNING_MESSAGES: [&str; 5] = [
    SKIPPING_SOURCE,
    ENCODE_FAILED,
    DID_NOT_FINISH,
    LEFT_PIPE_OPEN,
    COMMAND_FAILED,
];

/// One warning: core's `telemetry.Warn(ctx, message, attributes...)`.
///
/// The message is one of [`WARNING_MESSAGES`] and the fields are the attributes,
/// in the order core passes them. Until the telemetry slice owns a sink, this is
/// where a hook failure goes.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Warning {
    /// The log line's message: one of [`WARNING_MESSAGES`].
    pub message: &'static str,
    /// The log line's attributes, in the order core passes them.
    pub fields: Vec<(&'static str, String)>,
}

impl Warning {
    /// A warning from a message and its attributes.
    pub fn new<const N: usize>(
        message: &'static str,
        fields: [(&'static str, String); N],
    ) -> Warning {
        Warning {
            message,
            fields: fields.into_iter().collect(),
        }
    }

    /// One attribute's value, if it was passed.
    pub fn field(&self, name: &str) -> Option<&str> {
        self.fields
            .iter()
            .find(|(key, _)| *key == name)
            .map(|(_, value)| value.as_str())
    }
}

/// Where the runner's warnings go.
///
/// A trait rather than a `log` dependency: the telemetry slice will implement it
/// with the port's OTel sink, and a test implements it with a `Vec`. A closure
/// implements it too, which is what a caller that only wants to print them
/// writes.
pub trait Warn: Send + Sync {
    /// Record one warning.
    fn warn(&self, warning: &Warning);
}

impl<F: Fn(&Warning) + Send + Sync> Warn for F {
    fn warn(&self, warning: &Warning) {
        self(warning)
    }
}

impl<T: Warn + ?Sized> Warn for Arc<T> {
    fn warn(&self, warning: &Warning) {
        (**self).warn(warning)
    }
}

/// A sink that drops every warning: core's runner with no tracer configured.
pub struct Silent;

impl Warn for Silent {
    fn warn(&self, _warning: &Warning) {}
}

/// What one hook run retained of its combined output.
#[derive(Debug, Clone)]
pub struct Capture {
    limit: usize,
    bytes: Vec<u8>,
    truncated: bool,
}

impl Capture {
    /// A capture that keeps at most `limit` bytes.
    pub fn new(limit: usize) -> Capture {
        Capture {
            limit,
            bytes: Vec::new(),
            truncated: false,
        }
    }

    /// Append, keeping at most the limit and marking what was dropped.
    ///
    /// It keeps accepting writes after the limit -- a full pipe would block the
    /// hook -- and drops the excess, because the log line is the only consumer
    /// and it is bounded by the same limit.
    pub fn push(&mut self, data: &[u8]) {
        let room = self.limit.saturating_sub(self.bytes.len());
        if room == 0 {
            self.truncated = true;
            return;
        }
        if data.len() > room {
            self.bytes.extend_from_slice(&data[..room]);
            self.truncated = true;
            return;
        }
        self.bytes.extend_from_slice(data);
    }

    /// The captured output, marked when it was cut short.
    pub fn text(&self) -> String {
        let text = String::from_utf8_lossy(&self.bytes).into_owned();
        if self.truncated {
            return format!("{text}... (truncated)");
        }
        text
    }

    /// Whether anything was dropped.
    pub fn truncated(&self) -> bool {
        self.truncated
    }
}

/// Run one command hook with the event payload on stdin.
///
/// A failure is reported to `warn` and swallowed: see [`crate::Manager::fire`].
pub(crate) async fn run(
    event: &str,
    entry: &GroupEntry,
    hook: &Hook,
    payload: &Payload,
    warn: &dyn Warn,
) {
    let timeout = timeout_of(hook);
    let body = serde_json::Value::Object(event_body(event, payload, entry.trusted));
    let body = match serde_json::to_vec(&body) {
        Ok(body) => body,
        Err(err) => {
            warn.warn(&Warning::new(
                ENCODE_FAILED,
                [("event", event.to_owned()), ("error", err.to_string())],
            ));
            return;
        }
    };

    let mut command = tokio::process::Command::new(SHELL);
    command.arg("-c").arg(&hook.command);
    if !entry.dir.as_os_str().is_empty() {
        command.current_dir(&entry.dir);
    }
    command
        .stdin(std::process::Stdio::piped())
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped())
        // A future that is dropped mid-run takes its child with it: core's
        // context cancellation does the same, one step later.
        .kill_on_drop(true);
    super::exec::own_process_group(&mut command);

    let mut child = match command.spawn() {
        Ok(child) => child,
        Err(err) => {
            warn.warn(&Warning::new(
                COMMAND_FAILED,
                [
                    ("event", event.to_owned()),
                    ("command", hook.command.clone()),
                    ("error", err.to_string()),
                    ("output", String::new()),
                ],
            ));
            return;
        }
    };

    let output = Arc::new(Mutex::new(Capture::new(MAX_OUTPUT)));
    let mut readers = Vec::new();
    if let Some(stdout) = child.stdout.take() {
        readers.push(tokio::spawn(drain(stdout, Arc::clone(&output))));
    }
    if let Some(stderr) = child.stderr.take() {
        readers.push(tokio::spawn(drain(stderr, Arc::clone(&output))));
    }

    // The timeout bounds the write as well as the wait: core's context covers
    // `Cmd.Run`, stdin copy included, so a hook that never reads its stdin is
    // killed by the same deadline as one that never exits.
    let deadline = tokio::time::Instant::now() + timeout;
    if let Some(mut stdin) = child.stdin.take() {
        let _ = tokio::time::timeout_at(deadline, stdin.write_all(&body)).await;
        drop(stdin);
    }
    let mut timed_out = false;
    let status = match tokio::time::timeout_at(deadline, child.wait()).await {
        Ok(status) => status,
        Err(_) => {
            timed_out = true;
            super::exec::kill(&mut child);
            // The kill is a signal, not a guarantee: wait for the reaping, but
            // not forever.
            match tokio::time::timeout(WAIT_DELAY, child.wait()).await {
                Ok(status) => status,
                Err(_) => Err(std::io::Error::other("the hook did not die after SIGKILL")),
            }
        }
    };
    let pipe_open = tokio::time::timeout(WAIT_DELAY, async {
        for reader in readers {
            let _ = reader.await;
        }
    })
    .await
    .is_err();

    let captured = output
        .lock()
        .map(|capture| capture.text())
        .unwrap_or_default();
    let attrs = |error: String| {
        [
            ("event", event.to_owned()),
            ("command", hook.command.clone()),
            ("error", error),
            ("output", captured.clone()),
        ]
    };

    if timed_out {
        // The log has to survive the context that just expired; a sink has no
        // context to outlive.
        warn.warn(&Warning::new(
            DID_NOT_FINISH,
            attrs(format!("timed out after {timeout:?}")),
        ));
        return;
    }
    match status {
        Ok(status) if status.success() => {
            if pipe_open {
                warn.warn(&Warning::new(
                    LEFT_PIPE_OPEN,
                    attrs("the output pipe stayed open".to_owned()),
                ));
            }
        }
        Ok(status) => warn.warn(&Warning::new(COMMAND_FAILED, attrs(status.to_string()))),
        Err(err) => warn.warn(&Warning::new(COMMAND_FAILED, attrs(err.to_string()))),
    }
}

/// A hook's timeout: its own when it names one, the default otherwise.
fn timeout_of(hook: &Hook) -> Duration {
    if hook.timeout > 0 {
        return Duration::from_secs(hook.timeout.unsigned_abs());
    }
    DEFAULT_TIMEOUT
}

/// Read one pipe into the capture until it closes.
async fn drain<R>(mut pipe: R, capture: Arc<Mutex<Capture>>)
where
    R: tokio::io::AsyncRead + Unpin,
{
    let mut buffer = [0u8; 8 << 10];
    loop {
        match tokio::io::AsyncReadExt::read(&mut pipe, &mut buffer).await {
            Ok(0) => return,
            Ok(read) => {
                let mut capture = capture
                    .lock()
                    .unwrap_or_else(|poisoned| poisoned.into_inner());
                capture.push(&buffer[..read]);
            }
            Err(_) => return,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_capture_keeps_the_limit_and_says_so() {
        let mut capture = Capture::new(4);
        capture.push(b"abc");
        assert_eq!(capture.text(), "abc");
        assert!(!capture.truncated());
        capture.push(b"def");
        assert_eq!(capture.text(), "abcd... (truncated)");
        assert!(capture.truncated());
        // Still accepting writes after the limit, which is what keeps a hook
        // from blocking on a full pipe.
        capture.push(b"ghi");
        assert_eq!(capture.text(), "abcd... (truncated)");
    }

    #[test]
    fn a_capture_of_nothing_is_empty_and_unmarked() {
        assert_eq!(Capture::new(8).text(), "");
        assert!(!Capture::new(8).truncated());
    }

    #[test]
    fn a_timeout_is_the_hooks_own_or_the_default() {
        assert_eq!(timeout_of(&Hook::default()), DEFAULT_TIMEOUT);
        assert_eq!(
            timeout_of(&Hook {
                timeout: 0,
                ..Hook::default()
            }),
            DEFAULT_TIMEOUT
        );
        assert_eq!(
            timeout_of(&Hook {
                timeout: -5,
                ..Hook::default()
            }),
            DEFAULT_TIMEOUT
        );
        assert_eq!(
            timeout_of(&Hook {
                timeout: 2,
                ..Hook::default()
            }),
            Duration::from_secs(2)
        );
    }
}
