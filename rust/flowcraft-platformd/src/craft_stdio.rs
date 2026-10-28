//! Turning a craft's own output into records.
//!
//! A craft spawned by `exec` used to inherit the daemon's streams: its lines
//! landed in the middle of the daemon's log with no craft name, no session, and
//! in JSON mode a foreign line that breaks every parser downstream. This module
//! reads the pipes and re-emits each line as a record, attributed.
//!
//! Three properties matter more than throughput:
//!
//! * **One line is one record.** Binary noise, partial writes and invalid UTF-8
//!   must not split a record or leak control characters into the file.
//! * **A craft cannot flood the log.** A looping child would otherwise fill the
//!   disk of the machine it runs on; lines over the budget are dropped, and the
//!   count is reported once per window instead of once per line.
//! * **The parent never blocks.** Reading is a task per stream; if the child
//!   writes faster than it is read, the pipe applies backpressure to the child,
//!   which is the correct place for it.

use std::time::{Duration, Instant};

use flowcraft::kernel::config::CraftStdio;
use tokio::io::{AsyncRead, AsyncReadExt};
use tokio::process::Child;

/// The longest line kept; anything beyond is truncated and marked.
const MAX_LINE: usize = 8 * 1024;

/// How many lines one stream may emit per window.
const LINES_PER_WINDOW: u32 = 500;

/// The window a burst is measured over.
const WINDOW: Duration = Duration::from_secs(1);

/// Which stream a line came from.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum Stream {
    Stdout,
    Stderr,
}

impl Stream {
    const fn as_str(self) -> &'static str {
        match self {
            Self::Stdout => "stdout",
            Self::Stderr => "stderr",
        }
    }
}

/// Start reading a child's output, according to `policy`.
pub(crate) fn attach(craft: &str, child: &mut Child, policy: CraftStdio) {
    if policy != CraftStdio::Capture {
        return;
    }
    if let Some(stdout) = child.stdout.take() {
        spawn(craft.to_owned(), Stream::Stdout, stdout);
    }
    if let Some(stderr) = child.stderr.take() {
        spawn(craft.to_owned(), Stream::Stderr, stderr);
    }
}

fn spawn<R>(craft: String, stream: Stream, reader: R)
where
    R: AsyncRead + Unpin + Send + 'static,
{
    tokio::spawn(async move {
        pump(craft, stream, reader).await;
    });
}

/// Read `reader` to EOF, emitting one record per line.
async fn pump<R>(craft: String, stream: Stream, mut reader: R)
where
    R: AsyncRead + Unpin,
{
    let mut budget = Budget::new(craft, stream);
    let mut line = Vec::with_capacity(256);
    let mut truncated = false;
    let mut chunk = [0u8; 4096];

    loop {
        let read = match reader.read(&mut chunk).await {
            Ok(0) => break,
            Ok(read) => read,
            // The pipe closed under us: the craft is gone, and its last partial
            // line is not worth inventing a record for.
            Err(_) => return,
        };
        for byte in &chunk[..read] {
            if *byte == b'\n' {
                emit(&mut budget, &line, truncated);
                line.clear();
                truncated = false;
                continue;
            }
            // A carriage return before the newline belongs to the terminal
            // convention, not to the craft's message.
            if *byte == b'\r' {
                continue;
            }
            if line.len() < MAX_LINE {
                line.push(*byte);
            } else {
                truncated = true;
            }
        }
    }
    if !line.is_empty() || truncated {
        emit(&mut budget, &line, truncated);
    }
    budget.finish();
}

fn emit(budget: &mut Budget, line: &[u8], truncated: bool) {
    if line.is_empty() && !truncated {
        return;
    }
    if !budget.allow() {
        return;
    }
    let text = String::from_utf8_lossy(line);
    let craft = budget.craft.clone();
    let stream = budget.stream.as_str();
    if truncated {
        tracing::info!(
            flowcraft.craft = %craft,
            flowcraft.stream = stream,
            truncated = true,
            "{text}\u{2026}"
        );
    } else {
        tracing::info!(
            flowcraft.craft = %craft,
            flowcraft.stream = stream,
            "{text}"
        );
    }
}

/// A per-stream line budget.
struct Budget {
    craft: String,
    stream: Stream,
    window_start: Instant,
    used: u32,
    suppressed: u32,
}

impl Budget {
    fn new(craft: String, stream: Stream) -> Self {
        Self {
            craft,
            stream,
            window_start: Instant::now(),
            used: 0,
            suppressed: 0,
        }
    }

    /// Whether this line may be emitted, rolling the window first.
    fn allow(&mut self) -> bool {
        self.roll();
        if self.used < LINES_PER_WINDOW {
            self.used += 1;
            return true;
        }
        self.suppressed = self.suppressed.saturating_add(1);
        false
    }

    /// Report anything dropped since the last window closed.
    fn finish(&mut self) {
        self.report_suppressed();
    }

    fn roll(&mut self) {
        if self.window_start.elapsed() < WINDOW {
            return;
        }
        self.window_start = Instant::now();
        self.used = 0;
        self.report_suppressed();
    }

    fn report_suppressed(&mut self) {
        if self.suppressed == 0 {
            return;
        }
        let suppressed = std::mem::take(&mut self.suppressed);
        let craft = self.craft.as_str();
        let stream = self.stream.as_str();
        tracing::warn!(
            flowcraft.craft = %craft,
            flowcraft.stream = stream,
            suppressed,
            "craft output dropped: more than {LINES_PER_WINDOW} lines per second"
        );
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use flowcraft::telemetry::test_support::Capture;

    /// One pump run on the captured thread.
    async fn run(capture: &Capture, stream: Stream, input: &[u8]) {
        capture
            .with_async(pump("demo".to_owned(), stream, input))
            .await;
    }

    #[tokio::test(flavor = "current_thread")]
    async fn each_line_becomes_one_attributed_record() {
        let capture = Capture::new();
        run(&capture, Stream::Stdout, b"first\nsecond\n").await;

        let records = capture.records();
        assert_eq!(records.len(), 2, "{records:?}");
        assert_eq!(records[0].message, "first");
        assert_eq!(records[1].message, "second");
        assert_eq!(records[0].field("flowcraft.craft"), Some("demo"));
        assert_eq!(records[0].field("flowcraft.stream"), Some("stdout"));
        assert_eq!(records[0].level, tracing::Level::INFO);
    }

    #[tokio::test(flavor = "current_thread")]
    async fn a_trailing_line_without_a_newline_still_reaches_the_log() {
        let capture = Capture::new();
        run(&capture, Stream::Stderr, b"no newline at the end").await;
        let records = capture.records();
        assert_eq!(records.len(), 1);
        assert_eq!(records[0].message, "no newline at the end");
        assert_eq!(records[0].field("flowcraft.stream"), Some("stderr"));
    }

    #[tokio::test(flavor = "current_thread")]
    async fn carriage_returns_are_not_part_of_the_message() {
        let capture = Capture::new();
        run(&capture, Stream::Stdout, b"windows\r\nline\r\n").await;
        let messages: Vec<String> = capture.records().into_iter().map(|r| r.message).collect();
        assert_eq!(messages, vec!["windows", "line"]);
    }

    #[tokio::test(flavor = "current_thread")]
    async fn an_endless_line_is_truncated_and_marked() {
        let capture = Capture::new();
        let mut input = vec![b'x'; MAX_LINE * 2];
        input.push(b'\n');
        run(&capture, Stream::Stdout, &input).await;

        let records = capture.records();
        assert_eq!(records.len(), 1);
        assert_eq!(records[0].field("truncated"), Some("true"));
        assert!(
            records[0].message.ends_with('\u{2026}'),
            "{}",
            records[0].message
        );
        assert!(
            records[0].message.chars().count() <= MAX_LINE + 1,
            "message length {}",
            records[0].message.chars().count()
        );
    }

    #[tokio::test(flavor = "current_thread")]
    async fn invalid_utf8_does_not_break_the_record() {
        let capture = Capture::new();
        run(&capture, Stream::Stdout, b"before \xff\xfe after\n").await;
        let records = capture.records();
        assert_eq!(records.len(), 1);
        assert!(records[0].message.starts_with("before "));
        assert!(records[0].message.ends_with(" after"));
    }

    #[tokio::test(flavor = "current_thread")]
    async fn a_flood_is_capped_and_reported_once() {
        let capture = Capture::new();
        let mut input = Vec::new();
        for index in 0..(LINES_PER_WINDOW + 100) {
            input.extend(format!("line {index}\n").into_bytes());
        }
        run(&capture, Stream::Stdout, &input).await;

        let records = capture.records();
        let emitted = records
            .iter()
            .filter(|record| record.level == tracing::Level::INFO)
            .count();
        assert_eq!(emitted, LINES_PER_WINDOW as usize, "the budget is enforced");

        let summary = records
            .iter()
            .find(|record| record.level == tracing::Level::WARN)
            .expect("a suppression summary");
        assert_eq!(summary.field("suppressed"), Some("100"));
        assert_eq!(summary.field("flowcraft.craft"), Some("demo"));
    }

    #[tokio::test(flavor = "current_thread")]
    async fn empty_lines_are_dropped() {
        let capture = Capture::new();
        run(&capture, Stream::Stdout, b"\n\nkept\n\n").await;
        let messages: Vec<String> = capture.records().into_iter().map(|r| r.message).collect();
        assert_eq!(messages, vec!["kept"]);
    }
}
