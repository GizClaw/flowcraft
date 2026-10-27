// A test may reach the filesystem directly: it has no executor to starve.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::disallowed_methods)]

//! The Windows session semantics, on a real Windows host.
//!
//! The platform-agnostic suite in `sandbox_conformance.rs` runs everywhere. This
//! one exists because three promises can only be kept — and therefore only be
//! checked — with a job object:
//!
//! - `terminate` ends the **tree**, not just the leader;
//! - a memory cap is a kernel ceiling, not a sampling heuristic;
//! - a cap that ends a session reports `BudgetExceeded`, not a mystery exit code.
//!
//! Everything here is written through the public runner surface, so a regression
//! in the job wiring fails here rather than in a caller's build.

// A module rather than a crate-level `cfg(windows)`: `missing_docs` is denied on
// the command line and cannot be relaxed, and a crate that `cfg`s itself out has
// no docs left to find.
#[cfg(windows)]
mod windows {

    use std::path::Path;
    use std::sync::Arc;
    use std::time::Duration;

    use flowcraft::caps::sandbox::{
        ExitReason, LocalRunner, Runner, Session, SessionSpec, Signal, Stream,
    };
    use tokio_stream::StreamExt;

    fn runner(dir: &Path) -> Arc<dyn Runner> {
        Arc::new(LocalRunner::at(dir))
    }

    /// Read everything the session produced, up to EOF.
    async fn read_all(session: &Arc<dyn Session>) -> String {
        let mut out = String::new();
        let mut cursor = 0;
        for _ in 0..256 {
            let read = session.read(cursor, 64 * 1024).await.expect("read");
            cursor = read.next_seq;
            for chunk in &read.chunks {
                out.push_str(&String::from_utf8_lossy(&chunk.data));
            }
            if read.eof {
                break;
            }
        }
        out
    }

    /// A spec that runs `argv` with no tty.
    fn spec(argv: &[&str]) -> SessionSpec {
        SessionSpec::new(argv.iter().map(|arg| (*arg).to_owned()).collect())
    }

    #[tokio::test]
    async fn a_session_runs_and_reports_what_it_can_do() {
        let dir = tempfile::tempdir().expect("tempdir");
        let session = runner(dir.path())
            .start(spec(&["cmd", "/c", "echo hello-from-windows"]))
            .await
            .expect("start");
        let output = read_all(&session).await;
        assert!(
            output.contains("hello-from-windows"),
            "stdout must be captured: {output:?}"
        );
        let exit = session.wait().await.expect("wait");
        assert_eq!(exit.code, Some(0));
        assert_eq!(exit.reason, ExitReason::Exited);
        // Honesty about the session surface, not an aspiration: a pipe session has no
        // console to interrupt and no pty to resize.
        let features = session.features();
        assert!(!features.signal, "a pipe session cannot be signalled yet");
        assert!(!features.tty);
    }

    #[tokio::test]
    async fn terminate_ends_the_tree_not_only_the_leader() {
        let dir = tempfile::tempdir().expect("tempdir");
        let root = dir.path();
        // Two levels, both writing a heartbeat, so an orphan is visible as a file that
        // keeps growing after the session is gone. Written to disk as a batch file so
        // the quoting is not part of what is under test.
        std::fs::write(
            root.join("heartbeat.bat"),
            "@echo off\r\n\
         :loop\r\n\
         echo x >> heartbeat.txt\r\n\
         ping -n 1 -w 200 127.0.0.1 >NUL\r\n\
         goto loop\r\n",
        )
        .expect("write heartbeat");
        std::fs::write(
            root.join("tree.bat"),
            "@echo off\r\n\
         start /b cmd /c heartbeat.bat\r\n\
         cmd /c heartbeat.bat\r\n",
        )
        .expect("write tree");

        let session = runner(root)
            .start(spec(&["cmd", "/c", "tree.bat"]))
            .await
            .expect("start");
        // Let both levels start beating.
        tokio::time::sleep(Duration::from_millis(1200)).await;
        let before = std::fs::metadata(root.join("heartbeat.txt"))
            .map(|meta| meta.len())
            .unwrap_or(0);
        if before == 0 {
            // Self-diagnosing: a missing heartbeat says nothing on its own, and
            // the child's own output and exit code are what distinguish "the
            // batch never ran" from "it ran somewhere else".
            let output = read_all(&session).await;
            let exit = session.exit();
            let files = std::fs::read_dir(root)
                .map(|entries| {
                    entries
                        .filter_map(|entry| entry.ok().map(|e| e.file_name()))
                        .collect::<Vec<_>>()
                })
                .unwrap_or_default();
            panic!("the tree never started; files={files:?} exit={exit:?} output={output:?}");
        }

        session.terminate().await.expect("terminate");
        let exit = session.wait().await.expect("wait");
        assert_eq!(exit.reason, ExitReason::Terminated);

        // Nothing in the tree may still be writing: that is what the job buys.
        tokio::time::sleep(Duration::from_millis(400)).await;
        let settled = std::fs::metadata(root.join("heartbeat.txt"))
            .map(|meta| meta.len())
            .unwrap_or(0);
        tokio::time::sleep(Duration::from_millis(1500)).await;
        let later = std::fs::metadata(root.join("heartbeat.txt"))
            .map(|meta| meta.len())
            .unwrap_or(0);
        assert_eq!(
            (settled, later),
            (later, later),
            "a descendant outlived the session: {before} → {settled} → {later}"
        );
    }

    #[tokio::test]
    async fn a_memory_cap_ends_the_session_as_a_budget() {
        let dir = tempfile::tempdir().expect("tempdir");
        let mut spec = spec(&[
            "powershell",
            "-NoProfile",
            "-Command",
            // 256 MiB against a 64 MiB ceiling.
            "$a = New-Object byte[] 268435456; Write-Output done",
        ]);
        spec.opts.limits.memory_bytes = Some(64 * 1024 * 1024);
        spec.opts.timeout = Some(Duration::from_secs(30));

        let session = runner(dir.path()).start(spec).await.expect("start");
        let _ = read_all(&session).await;
        let exit = session.wait().await.expect("wait");
        assert_eq!(
            exit.reason,
            ExitReason::BudgetExceeded,
            "a killed-by-cap session must say so: {exit:?}"
        );
    }

    #[tokio::test]
    async fn a_cpu_budget_ends_the_session_as_a_budget() {
        let dir = tempfile::tempdir().expect("tempdir");
        let mut spec = spec(&[
            "powershell",
            "-NoProfile",
            "-Command",
            "$end = (Get-Date).AddSeconds(30); while ((Get-Date) -lt $end) { $null = 1 + 1 }",
        ]);
        // One thousandth of a core over ten seconds: 10 ms of cpu time, which a
        // shell's own startup already exceeds. The budget has to be small enough
        // to fire *before* the deadline, or `TimedOut` wins the record and the
        // test would be asserting the wrong mechanism.
        spec.opts.limits.cpu_millicores = Some(1);
        spec.opts.timeout = Some(Duration::from_secs(10));

        let session = runner(dir.path()).start(spec).await.expect("start");
        let _ = read_all(&session).await;
        let exit = session.wait().await.expect("wait");
        assert_eq!(
            exit.reason,
            ExitReason::BudgetExceeded,
            "a cpu budget must be reported as one: {exit:?}"
        );
    }

    /// The same spec, but asking for a terminal.
    fn tty_spec(argv: &[&str]) -> SessionSpec {
        let mut spec = spec(argv);
        spec.tty = true;
        spec
    }

    #[tokio::test]
    async fn a_pty_session_merges_both_streams_into_one() {
        let dir = tempfile::tempdir().expect("tempdir");
        let session = runner(dir.path())
            .start(tty_spec(&["cmd", "/c", "echo to-out & echo to-err 1>&2"]))
            .await
            .expect("start");
        let output = read_all(&session).await;
        let exit = session.wait().await.expect("wait");
        assert_eq!(exit.code, Some(0), "{exit:?}");
        assert_eq!(exit.reason, ExitReason::Exited);
        // A terminal is one stream: that is what makes `Stream::Tty` a shape
        // rather than a label.
        assert!(output.contains("to-out"), "{output:?}");
        assert!(output.contains("to-err"), "{output:?}");
        let features = session.features();
        assert!(features.tty, "the session must declare its terminal");
        assert!(features.signal, "a terminal can be interrupted");
    }

    #[tokio::test]
    async fn a_pty_session_can_be_resized() {
        let dir = tempfile::tempdir().expect("tempdir");
        // Nothing here asserts pixel-perfect reflow: the claim is that resize
        // reaches a live console instead of being refused.
        let mut spec = tty_spec(&["cmd", "/c", "ping -n 4 127.0.0.1 >NUL"]);
        spec.rows = 24;
        spec.cols = 80;
        let session = runner(dir.path()).start(spec).await.expect("start");
        session.resize(40, 120).await.expect("resize");
        assert_eq!(session.wait().await.expect("wait").code, Some(0));
    }

    #[tokio::test]
    async fn ctrl_c_ends_a_pty_session() {
        let dir = tempfile::tempdir().expect("tempdir");
        // `ping` runs until interrupted, and a console app dies on Ctrl-C.
        let session = runner(dir.path())
            .start(tty_spec(&["ping", "-n", "60", "127.0.0.1"]))
            .await
            .expect("start");
        tokio::time::sleep(Duration::from_millis(800)).await;
        session.signal(Signal::Interrupt).await.expect("signal");
        // A 60-second command returning proves the byte became a console event.
        let exit = tokio::time::timeout(Duration::from_secs(15), session.wait())
            .await
            .expect("the session must end after Ctrl-C")
            .expect("wait");
        assert!(
            exit.code.unwrap_or_default() != 0,
            "an interrupted console app does not exit successfully: {exit:?}"
        );
    }

    #[tokio::test]
    async fn terminate_ends_a_pty_tree() {
        let dir = tempfile::tempdir().expect("tempdir");
        let root = dir.path();
        std::fs::write(
            root.join("heartbeat.bat"),
            "@echo off\r\n:loop\r\necho x >> heartbeat.txt\r\n             ping -n 1 -w 200 127.0.0.1 >NUL\r\ngoto loop\r\n",
        )
        .expect("write heartbeat");
        let session = runner(root)
            .start(tty_spec(&["cmd", "/c", "heartbeat.bat"]))
            .await
            .expect("start");
        tokio::time::sleep(Duration::from_millis(1200)).await;
        session.terminate().await.expect("terminate");
        let exit = session.wait().await.expect("wait");
        assert_eq!(exit.reason, ExitReason::Terminated);
        tokio::time::sleep(Duration::from_millis(400)).await;
        let settled = std::fs::metadata(root.join("heartbeat.txt"))
            .map(|meta| meta.len())
            .unwrap_or(0);
        tokio::time::sleep(Duration::from_millis(1500)).await;
        let later = std::fs::metadata(root.join("heartbeat.txt"))
            .map(|meta| meta.len())
            .unwrap_or(0);
        assert_eq!(settled, later, "a pty descendant outlived the session");
    }

    #[tokio::test]
    async fn a_pty_session_with_a_memory_cap_reports_a_budget() {
        let dir = tempfile::tempdir().expect("tempdir");
        let mut spec = tty_spec(&[
            "powershell",
            "-NoProfile",
            "-Command",
            "$a = New-Object byte[] 268435456; Write-Output done",
        ]);
        spec.opts.limits.memory_bytes = Some(64 * 1024 * 1024);
        spec.opts.timeout = Some(Duration::from_secs(30));
        let session = runner(dir.path()).start(spec).await.expect("start");
        let _ = read_all(&session).await;
        let exit = session.wait().await.expect("wait");
        assert_eq!(
            exit.reason,
            ExitReason::BudgetExceeded,
            "a capped pty session reports the cap, not a mystery exit: {exit:?}"
        );
    }

    #[tokio::test]
    async fn a_capped_session_still_reports_its_streams() {
        let dir = tempfile::tempdir().expect("tempdir");
        let mut spec = spec(&["cmd", "/c", "echo out & echo err 1>&2 & exit /b 3"]);
        // A cap that is never reached must not change the reporting.
        spec.opts.limits.memory_bytes = Some(512 * 1024 * 1024);

        let session = runner(dir.path()).start(spec).await.expect("start");
        let mut streams = Vec::new();
        let mut events = session.watch().expect("watch");
        let collecting = async {
            while let Some(event) = events.next().await {
                if let flowcraft::caps::sandbox::Event::Output { stream, .. } = event {
                    streams.push(stream);
                }
            }
        };
        let _ = tokio::time::timeout(Duration::from_secs(20), collecting).await;
        let exit = session.wait().await.expect("wait");
        assert_eq!(exit.code, Some(3));
        assert_eq!(exit.reason, ExitReason::Exited);
        assert!(
            streams.contains(&Stream::Stdout) && streams.contains(&Stream::Stderr),
            "both streams must be reported: {streams:?}"
        );
    }
}
