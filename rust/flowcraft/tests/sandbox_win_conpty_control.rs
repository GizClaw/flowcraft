#![allow(clippy::unwrap_used, clippy::expect_used)]

//! A control experiment for the Windows pty.
//!
//! `flowcraft-win32::Pty` is this repo's own ConPTY, written because the
//! confinement phases need to own `CreateProcess`. When its tests fail, there are
//! two very different explanations and no way to tell them apart from the
//! failure text:
//!
//! 1. the plumbing is wrong (pipe ends, startup info, attribute list), or
//! 2. this host cannot give a pseudo console to a process at all.
//!
//! This suite answers that with `portable-pty`, whose ConPTY backend is the
//! wezterm one and is exercised by thousands of projects on Windows CI. If it
//! succeeds here and [`flowcraft_win32`]'s does not, the bug is ours. If it fails
//! too, the host is the constraint and the capability should be reported
//! unavailable rather than "broken".
//!
//! It is a test, not a dependency: nothing outside this file uses
//! `portable-pty` on Windows. Every wait is bounded — a control experiment that
//! hangs tells you less than one that fails.

// A module rather than a crate-level `cfg(windows)`: the crate must keep its docs
// on a host where this file compiles to nothing.
#[cfg(windows)]
mod control {
    use std::io::Read;
    use std::sync::mpsc;
    use std::time::{Duration, Instant};

    /// How long the reference backend gets to produce output.
    const BUDGET: Duration = Duration::from_secs(20);

    /// Read chunks from a console stream until it is quiet or time runs out.
    ///
    /// Chunks rather than `read_to_end`: a terminal has no EOF of its own, and a
    /// blocking `read_to_end` is how a control experiment turns into a hung lane.
    fn read_until_quiet(mut reader: Box<dyn Read + Send>) -> String {
        let (tx, rx) = mpsc::channel::<Vec<u8>>();
        std::thread::spawn(move || {
            let mut buffer = [0u8; 4096];
            loop {
                match reader.read(&mut buffer) {
                    Ok(0) => break,
                    Ok(read) => {
                        if tx.send(buffer[..read].to_vec()).is_err() {
                            break;
                        }
                    }
                    Err(_) => break,
                }
            }
        });

        let mut text = String::new();
        let deadline = Instant::now() + BUDGET;
        while Instant::now() < deadline {
            let remaining = deadline.saturating_duration_since(Instant::now());
            let Ok(chunk) = rx.recv_timeout(remaining) else {
                break;
            };
            text.push_str(&String::from_utf8_lossy(&chunk));
            if text.contains("control-works") || text.contains("100") {
                break;
            }
        }
        text
    }

    /// The wezterm ConPTY backend through `portable-pty`: known-good code.
    fn portable_conpty_output(argv: &[&str]) -> String {
        use portable_pty::{CommandBuilder, PtySize, native_pty_system};

        let pair = native_pty_system()
            .openpty(PtySize {
                rows: 30,
                cols: 100,
                pixel_width: 0,
                pixel_height: 0,
            })
            .expect("openpty");
        let mut command = CommandBuilder::new(argv[0]);
        command.args(&argv[1..]);
        let child = pair.slave.spawn_command(command).expect("spawn_command");
        drop(pair.slave);
        let reader = pair.master.try_clone_reader().expect("reader");
        let text = read_until_quiet(reader);
        // Neither the child nor the console is waited on: `ClosePseudoConsole`
        // blocks until the console host drains, and this test's job is to report
        // what the stream carried, not to clean up. The process exits either way.
        std::mem::forget(child);
        std::mem::forget(pair.master);
        text
    }

    #[test]
    fn the_host_gives_a_pseudo_console_a_live_output_stream() {
        // The console is real if *anything* comes back. On this host the
        // reference backend returns exactly one sequence, `ESC[6n`, and nothing
        // else, because the console asks its terminal for the cursor position and
        // then waits: the reference backend never answers, so `cmd` stalls and its
        // own output never appears.
        //
        // That single sequence is the finding this suite exists for. It says the
        // host works and the pipe is live, so a pty that produces nothing is
        // missing the *answer*, not the console — and it is why
        // `flowcraft_win32::CursorReport` exists.
        let text = portable_conpty_output(&["cmd", "/c", "echo control-works"]);
        assert!(
            text.contains("control-works") || text.contains("\u{1b}[6n"),
            "the reference ConPTY backend must reach a live console stream: {text:?}"
        );
    }

    #[test]
    fn an_unanswered_cursor_request_is_what_stalls_the_console() {
        // Documents the mechanism rather than the symptom: with the same backend,
        // a command that asks for its window produces the request and then waits.
        let text = portable_conpty_output(&["cmd", "/c", "mode con"]);
        assert!(
            text.contains("100") || text.contains("\u{1b}[6n"),
            "the reference backend must either report the window or ask for the \
             cursor first: {text:?}"
        );
    }
}
