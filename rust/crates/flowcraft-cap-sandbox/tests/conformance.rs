#![allow(clippy::unwrap_used, clippy::expect_used)]

//! One suite, every backend.
//!
//! The assertions are the session semantics, not a backend's implementation, so
//! the same list runs against `local`, `bwrap`, `seatbelt`, and the remote
//! client. A backend that cannot run on this host skips, and a backend that
//! breaks a rule fails with its own name in the message.
//!
//! What is deliberately *not* here: anything that needs a real network or a
//! writable filesystem boundary. Those belong to the plan's later phases (the
//! enforcement proxy and the runner root); today the suite pins the properties
//! a caller can already rely on.

mod support;

use std::sync::Arc;
use std::time::Duration;

use flowcraft_cap_sandbox::{
    ConfinedRunner, Event, ExitReason, LocalRunner, NetMode, Runner, Sandbox, SessionSpec, Signal,
    Stream, WriteMode,
};
use tokio_stream::StreamExt;

fn local() -> Option<Arc<dyn Runner>> {
    Some(Arc::new(LocalRunner::new()))
}

fn confined(sandbox: Sandbox, binary: &str) -> Option<Arc<dyn Runner>> {
    if std::process::Command::new(binary)
        .arg("--version")
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .status()
        .is_err()
    {
        eprintln!(
            "skipping {}: {binary} is not usable here",
            sandbox.impl_name()
        );
        return None;
    }
    Some(Arc::new(ConfinedRunner::new(sandbox, Some(binary))))
}

#[tokio::test]
async fn local_conforms() {
    if let Some(runner) = local() {
        suite("local", runner).await;
    }
}

#[tokio::test]
async fn seatbelt_conforms() {
    if let Some(runner) = confined(Sandbox::Seatbelt, "sandbox-exec") {
        suite("seatbelt", runner).await;
    }
}

#[tokio::test]
async fn bwrap_conforms() {
    if let Some(runner) = confined(Sandbox::Bwrap, "bwrap") {
        suite("bwrap", runner).await;
    }
}

#[tokio::test]
async fn remote_conforms() {
    let stack = support::stack(&["sh", "cat", "sleep"]).await;
    let runner = support::remote_runner(&stack);
    suite("remote", runner).await;
    stack.built.shutdown().await.expect("shutdown");
}

/// The whole list, in one place.
async fn suite(backend: &str, runner: Arc<dyn Runner>) {
    read_blocks_until_output(backend, &*runner).await;
    a_byte_cursor_replays(backend, &*runner).await;
    streams_stay_separate(backend, &*runner).await;
    an_undeclared_input_side_is_not_available(backend, &*runner).await;
    close_input_without_stdin_is_a_no_op(backend, &*runner).await;
    exit_codes_and_signals_are_classified(backend, &*runner).await;
    a_timeout_is_classified(backend, &*runner).await;
    terminate_ends_the_group(backend, &*runner).await;
    watch_replays_then_streams(backend, &*runner).await;
    closing_releases_the_handle(backend, &*runner).await;
    listing_reflects_lifecycle(backend, &*runner).await;
    capabilities_never_overclaim(backend, &*runner).await;
    runner.close().await.expect("runner close");
}

async fn read_blocks_until_output(backend: &str, runner: &dyn Runner) {
    let session = runner
        .start(support::spec(&["sh", "-c", "sleep 0.15; echo late"]))
        .await
        .expect("start");
    let reader = {
        let session = session.clone();
        tokio::spawn(async move { session.read(0, 64 * 1024).await })
    };
    tokio::time::sleep(Duration::from_millis(50)).await;
    assert!(
        !reader.is_finished(),
        "{backend}: read returned before there was output"
    );
    let read = reader.await.expect("join").expect("read");
    assert!(
        read.next_seq > 0,
        "{backend}: a completed read must advance the cursor"
    );
    session.close().await.expect("close");
}

async fn a_byte_cursor_replays(backend: &str, runner: &dyn Runner) {
    let session = runner
        .start(support::spec(&["sh", "-c", "printf 'abcdefghij'"]))
        .await
        .expect("start");
    // Drain in small steps, so the cursor lands mid-chunk at least once.
    let mut text = String::new();
    let mut cursor = 0;
    loop {
        let read = session.read(cursor, 3).await.expect("read");
        assert!(read.chunks.len() <= 1, "{backend}: batch exceeded the ask");
        for chunk in &read.chunks {
            assert_eq!(
                chunk.seq, cursor,
                "{backend}: a chunk must start at the requested cursor"
            );
            text.push_str(&String::from_utf8_lossy(&chunk.data));
        }
        cursor = read.next_seq;
        if read.eof {
            break;
        }
    }
    assert_eq!(
        text, "abcdefghij",
        "{backend}: replay lost or reordered bytes"
    );
    // The retained window stays readable after the process is gone.
    let (_, again) = support::read_all_from(&session, 0).await;
    assert_eq!(again, "abcdefghij", "{backend}: replay after exit");
    session.close().await.expect("close");
}

async fn streams_stay_separate(backend: &str, runner: &dyn Runner) {
    let session = runner
        .start(support::spec(&["sh", "-c", "echo out; echo err 1>&2"]))
        .await
        .expect("start");
    session.wait().await.expect("wait");
    let mut stdout = String::new();
    let mut stderr = String::new();
    let mut cursor = 0;
    loop {
        let read = session.read(cursor, 4096).await.expect("read");
        for chunk in &read.chunks {
            let text = String::from_utf8_lossy(&chunk.data).into_owned();
            match chunk.stream {
                Stream::Stdout => stdout.push_str(&text),
                Stream::Stderr => stderr.push_str(&text),
                Stream::Tty => panic!("{backend}: a pipe session produced a tty chunk"),
            }
        }
        cursor = read.next_seq;
        if read.eof {
            break;
        }
    }
    assert_eq!(stdout, "out\n", "{backend}: stdout");
    assert_eq!(stderr, "err\n", "{backend}: stderr");
    session.close().await.expect("close");
}

async fn an_undeclared_input_side_is_not_available(backend: &str, runner: &dyn Runner) {
    let session = runner
        .start(support::spec(&["sh", "-c", "true"]))
        .await
        .expect("start");
    let error = session.write(b"x").await.expect_err("no input side");
    assert_eq!(
        error.kind(),
        flowcraft_error::ErrorKind::NotAvailable,
        "{backend}: write without stdin: {error}"
    );
    session.close().await.expect("close");
}

async fn close_input_without_stdin_is_a_no_op(backend: &str, runner: &dyn Runner) {
    let session = runner
        .start(support::spec(&["sh", "-c", "true"]))
        .await
        .expect("start");
    session
        .close_input()
        .await
        .unwrap_or_else(|error| panic!("{backend}: close_input without stdin: {error}"));
    session.close().await.expect("close");
}

async fn exit_codes_and_signals_are_classified(backend: &str, runner: &dyn Runner) {
    let session = runner
        .start(support::spec(&["sh", "-c", "exit 3"]))
        .await
        .expect("start");
    let exit = session.wait().await.expect("wait");
    assert_eq!(exit.code, Some(3), "{backend}: exit code");
    assert_eq!(exit.reason, ExitReason::Exited, "{backend}: exit reason");
    session.close().await.expect("close");

    let session = runner
        .start(support::spec(&["sh", "-c", "kill -TERM $$"]))
        .await
        .expect("start");
    let exit = session.wait().await.expect("wait");
    assert_eq!(exit.reason, ExitReason::Signaled, "{backend}: signaled");
    assert_eq!(exit.signal, Some(15), "{backend}: terminating signal");
    session.close().await.expect("close");
}

async fn a_timeout_is_classified(backend: &str, runner: &dyn Runner) {
    let mut request = support::spec(&["sleep", "30"]);
    // The grant ceiling is 5s, so this stays inside it.
    request.opts.timeout = Some(Duration::from_millis(120));
    let session = runner.start(request).await.expect("start");
    // A remote backend reports the classification as a failed call (the Go
    // client's `Wait` contract); an in-process one returns the outcome and
    // exposes the same classification through `error()`.
    match session.wait().await {
        Ok(exit) => {
            assert_eq!(
                exit.reason,
                ExitReason::TimedOut,
                "{backend}: timeout reason"
            );
            assert_eq!(
                exit.error().map(|error| error.kind()),
                Some(flowcraft_error::ErrorKind::Timeout),
                "{backend}: timeout classification"
            );
        }
        Err(error) => assert_eq!(
            error.kind(),
            flowcraft_error::ErrorKind::Timeout,
            "{backend}: timeout over the wire: {error}"
        ),
    }
    session.close().await.expect("close");
}

async fn terminate_ends_the_group(backend: &str, runner: &dyn Runner) {
    let session = runner
        .start(support::spec(&["sleep", "30"]))
        .await
        .expect("start");
    session.terminate().await.expect("terminate");
    let exit = session.wait().await.expect("wait");
    assert_eq!(
        exit.reason,
        ExitReason::Terminated,
        "{backend}: intended termination must be reported as such"
    );
    session.close().await.expect("close");
}

async fn watch_replays_then_streams(backend: &str, runner: &dyn Runner) {
    let session = runner
        .start(support::spec(&["sh", "-c", "echo watched"]))
        .await
        .expect("start");
    let mut events = session.watch().expect("watch");
    let mut text = String::new();
    let mut exited = false;
    while let Some(event) = events.next().await {
        match event {
            Event::Output { data, .. } => {
                text.push_str(&String::from_utf8_lossy(&data));
            }
            Event::Exited { .. } => {
                exited = true;
                break;
            }
            Event::Closed { .. } | Event::Lag { .. } => break,
        }
    }
    assert!(text.contains("watched"), "{backend}: watch output {text:?}");
    assert!(exited, "{backend}: watch must deliver the exit event");
    session.close().await.expect("close");
}

async fn closing_releases_the_handle(backend: &str, runner: &dyn Runner) {
    let session = runner
        .start(support::spec(&["sleep", "30"]))
        .await
        .expect("start");
    session.close().await.expect("close");
    for error in [
        session.read(0, 16).await.expect_err("read after close"),
        session.write(b"x").await.expect_err("write after close"),
        session
            .signal(Signal::Interrupt)
            .await
            .expect_err("signal after close"),
    ] {
        assert_eq!(
            error.kind(),
            flowcraft_error::ErrorKind::Closed,
            "{backend}: {error}"
        );
    }
    // Close is idempotent.
    session.close().await.expect("idempotent close");
}

async fn listing_reflects_lifecycle(backend: &str, runner: &dyn Runner) {
    let session = runner
        .start(support::spec(&["sh", "-c", "exit 0"]))
        .await
        .expect("start");
    let id = session.id().to_owned();
    session.wait().await.expect("wait");
    let listed = runner.list().await.expect("list");
    let info = listed
        .iter()
        .find(|info| info.id == id)
        .unwrap_or_else(|| panic!("{backend}: {id} is not listed"));
    assert!(!info.running, "{backend}: a finished session must not run");
    assert!(info.exit.is_some(), "{backend}: the outcome is reported");
    runner.close_session(&id).await.expect("close_session");
    assert!(
        !runner
            .list()
            .await
            .expect("list")
            .iter()
            .any(|info| info.id == id),
        "{backend}: a released session must disappear from the list"
    );
}

async fn capabilities_never_overclaim(backend: &str, runner: &dyn Runner) {
    let capabilities = runner.capabilities().await.expect("capabilities");
    let policy = &capabilities.policy;

    // A declaration is a promise: whatever is not claimed must be refused.
    if !policy.net.deny_all {
        let mut request = support::spec(&["sh", "-c", "true"]);
        request.opts.net.mode = NetMode::DenyAll;
        let error = runner
            .start(request)
            .await
            .err()
            .unwrap_or_else(|| panic!("{backend}: deny_all was claimed as unsupported"));
        assert_eq!(
            error.kind(),
            flowcraft_error::ErrorKind::NotAvailable,
            "{backend}: deny_all must be refused with NotAvailable"
        );
    }
    if !policy.write.read_only {
        let mut request = support::spec(&["sh", "-c", "true"]);
        request.opts.write = WriteMode::ReadOnly;
        let error = runner
            .start(request)
            .await
            .err()
            .unwrap_or_else(|| panic!("{backend}: read_only was claimed as unsupported"));
        assert_eq!(
            error.kind(),
            flowcraft_error::ErrorKind::NotAvailable,
            "{backend}: read_only must be refused with NotAvailable"
        );
    }
    // A backend cannot confine writes to a workspace it does not have.
    if !policy.filesystem_bounds {
        assert!(
            !policy.write.workspace,
            "{backend}: a writable-subtree claim without filesystem bounds"
        );
    }
    // `stdin` is per session, never a runner-level promise.
    assert!(
        !capabilities.features.stdin,
        "{backend}: stdin is decided by the session spec"
    );
}

/// A spec built by hand keeps the suite honest about the defaults.
#[test]
fn a_default_spec_is_not_interactive() {
    let spec: SessionSpec = support::spec(&["true"]);
    assert!(!spec.tty);
    assert!(!spec.wants_stdin());
    assert_eq!(spec.window(), (24, 80));
}
