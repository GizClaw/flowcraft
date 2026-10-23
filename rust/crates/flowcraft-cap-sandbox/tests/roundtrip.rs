#![allow(clippy::unwrap_used, clippy::expect_used)]

//! End-to-end: local runner → endpoint server → remote client, over a real unix
//! socket, with grants and a command allow-list.
//!
//! These tests pin the wire behaviour the Go client relies on: the three RPCs
//! that used to answer `UNIMPLEMENTED` (`Resize` / `Signal` / `Watch`), the
//! single session table behind `close_session`, and the exit classification
//! travelling across the boundary.

mod support;

use std::sync::Arc;
use std::time::Duration;

use flowcraft_cap_sandbox::{Event, ExitReason, Runner, Signal, Stream};
use tokio_stream::StreamExt;

fn client(stack: &support::Stack) -> Arc<dyn Runner> {
    support::remote_runner(stack)
}

#[tokio::test]
async fn runs_a_command_through_the_endpoint() {
    let stack = support::stack(&["sh"]).await;
    let runner = client(&stack);
    let session = runner
        .start(support::spec(&["sh", "-c", "echo hello; exit 3"]))
        .await
        .expect("start");
    let exit = session.wait().await.expect("wait");
    assert_eq!(exit.code, Some(3));
    assert_eq!(exit.reason, ExitReason::Exited);
    assert!(support::read_all(&session).await.contains("hello"));
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn stdin_round_trips() {
    let stack = support::stack(&["cat"]).await;
    let runner = client(&stack);
    let mut request = support::spec(&["cat"]);
    request.stdin = true;
    let session = runner.start(request).await.expect("start");
    assert!(session.features().stdin, "stdin is a per-session answer");
    session.write(b"ping\n").await.expect("write");
    session.close_input().await.expect("close");
    let exit = session.wait().await.expect("wait");
    assert_eq!(exit.code, Some(0));
    assert!(support::read_all(&session).await.contains("ping"));
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn the_command_allow_list_is_enforced() {
    let stack = support::stack(&["sh"]).await;
    let runner = client(&stack);
    let error = runner
        .start(support::spec(&[
            "rm",
            "-rf",
            "/tmp/flowcraft-should-not-run",
        ]))
        .await
        .expect_err("denied");
    assert_eq!(error.kind(), flowcraft_error::ErrorKind::Forbidden);
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn a_timeout_is_classified_across_the_wire() {
    let stack = support::stack(&["sleep"]).await;
    let runner = client(&stack);
    let mut request = support::spec(&["sleep", "30"]);
    request.opts.timeout = Some(Duration::from_millis(100));
    let session = runner.start(request).await.expect("start");
    // The server reports the classification as a failed call, which is what the
    // Go client's `Wait` expects.
    let error = session.wait().await.expect_err("timeout");
    assert_eq!(error.kind(), flowcraft_error::ErrorKind::Timeout);
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn a_timeout_above_the_grant_ceiling_is_refused() {
    let stack = support::stack(&["sleep"]).await;
    let runner = client(&stack);
    let mut request = support::spec(&["sleep", "30"]);
    request.opts.timeout = Some(Duration::from_secs(60));
    let error = runner.start(request).await.expect_err("ceiling");
    assert_eq!(error.kind(), flowcraft_error::ErrorKind::Forbidden);
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn sessions_are_listed() {
    let stack = support::stack(&["sh"]).await;
    let runner = client(&stack);
    let session = runner
        .start(support::spec(&["sh", "-c", "exit 0"]))
        .await
        .expect("start");
    let id = session.id().to_owned();
    session.wait().await.expect("wait");
    let infos = runner.list().await.expect("list");
    assert!(infos.iter().any(|info| info.id == id), "{infos:?}");
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn capabilities_come_from_the_server() {
    let stack = support::stack(&["sh"]).await;
    let runner = client(&stack);
    let capabilities = runner.capabilities().await.expect("capabilities");
    assert!(capabilities.policy.env_allow_list);
    // The local backend has no OS boundary, so it must not claim one.
    assert!(!capabilities.policy.filesystem_bounds);
    assert!(!capabilities.features.tty);
    // `stdin` is decided per session, so a runner-level `true` would be wrong.
    assert!(!capabilities.features.stdin);
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn resize_reaches_the_session_instead_of_answering_unimplemented() {
    let stack = support::stack(&["sleep"]).await;
    let runner = client(&stack);
    let session = runner
        .start(support::spec(&["sleep", "30"]))
        .await
        .expect("start");
    let error = session.resize(40, 120).await.expect_err("no pty");
    // A pipe session cannot be resized. The important part is that the RPC is
    // served: `UNIMPLEMENTED` would be reported as `NotAvailable` with the
    // transport's message, and `NotAvailable` here is the backend's own answer.
    assert_eq!(error.kind(), flowcraft_error::ErrorKind::NotAvailable);
    assert!(error.to_string().contains("pty"), "{error}");
    session.close().await.expect("close");
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn signal_reaches_the_session() {
    let stack = support::stack(&["sh"]).await;
    let runner = client(&stack);
    let session = runner
        .start(support::spec(&[
            "sh",
            "-c",
            "trap 'exit 0' INT; while :; do sleep 0.05; done",
        ]))
        .await
        .expect("start");
    // Give the shell a moment to install the trap.
    tokio::time::sleep(Duration::from_millis(150)).await;
    session.signal(Signal::Interrupt).await.expect("signal");
    let exit = session.wait().await.expect("wait");
    assert_eq!(exit.code, Some(0), "{exit:?}");
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn watch_streams_output_and_exit() {
    let stack = support::stack(&["sh"]).await;
    let runner = client(&stack);
    let session = runner
        .start(support::spec(&["sh", "-c", "echo watched"]))
        .await
        .expect("start");
    let mut events = session.watch().expect("watch");
    let mut output = String::new();
    let mut saw_exit = false;
    while let Some(event) = events.next().await {
        match event {
            Event::Output { stream, data, .. } => {
                assert_eq!(stream, Stream::Stdout);
                output.push_str(&String::from_utf8_lossy(&data));
            }
            Event::Exited { .. } => saw_exit = true,
            Event::Closed { .. } | Event::Lag { .. } => break,
        }
        if saw_exit {
            break;
        }
    }
    assert!(output.contains("watched"), "{output:?}");
    assert!(saw_exit, "the exit event must be pushed");
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn closing_a_session_forgets_it_everywhere() {
    let stack = support::stack(&["sh"]).await;
    let runner = client(&stack);
    let session = runner
        .start(support::spec(&["sh", "-c", "exit 0"]))
        .await
        .expect("start");
    let id = session.id().to_owned();
    session.wait().await.expect("wait");
    runner.close_session(&id).await.expect("close_session");
    assert_eq!(
        runner.session(&id).await.err().unwrap().kind(),
        flowcraft_error::ErrorKind::NotFound
    );
    assert!(
        !runner
            .list()
            .await
            .expect("list")
            .iter()
            .any(|info| info.id == id)
    );
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn a_workdir_escape_is_refused_across_the_wire() {
    let stack = support::stack(&["sh"]).await;
    let runner = client(&stack);
    let mut request = support::spec(&["sh", "-c", "true"]);
    request.opts.workdir = Some(std::path::PathBuf::from(".."));
    let error = runner.start(request).await.expect_err("traversal");
    assert_eq!(error.kind(), flowcraft_error::ErrorKind::Forbidden);
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn a_journal_round_trips_over_the_wire() {
    let stack = support::stack(&["sh"]).await;
    let runner = client(&stack);
    let capabilities = runner.capabilities().await.expect("capabilities");
    assert!(
        capabilities.journal.enabled,
        "the server declared a journal"
    );

    let journal = runner.open_journal().await.expect("open_journal");
    // A file written under the runner root must show up as one create.
    std::fs::write(stack.dir.path().join("root/artifact.txt"), b"x").expect("write");

    let mut cursor = 0;
    let mut ops = Vec::new();
    for _ in 0..40 {
        let batch = journal.read(cursor, 64).await.expect("read");
        assert!(batch.gap.is_none(), "no coverage loss expected: {batch:?}");
        cursor = batch.next_seq;
        ops.extend(batch.events.iter().map(|event| event.op));
        if !ops.is_empty() {
            break;
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
    journal.close().await.expect("close_journal");
    assert!(
        ops.contains(&flowcraft_cap_sandbox::journal::FileOp::Create),
        "the creation must travel: {ops:?}"
    );
    // A closed reader refuses further reads.
    assert_eq!(
        journal.read(cursor, 8).await.expect_err("closed").kind(),
        flowcraft_error::ErrorKind::Closed
    );
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn a_revoked_session_cannot_start_a_process() {
    let stack = support::stack(&["sh"]).await;
    stack
        .platform
        .revoke_session(stack.platform.session())
        .expect("revoke");
    let runner = client(&stack);
    let error = runner
        .start(support::spec(&["sh", "-c", "true"]))
        .await
        .expect_err("revoked");
    assert_eq!(error.kind(), flowcraft_error::ErrorKind::Forbidden);
    // Revocation cuts the whole control plane, including this client's
    // `CloseRunner`. Shutdown still succeeds: the endpoint drains first, and a
    // client that cannot reach its endpoint has nothing left to close.
    stack.built.shutdown().await.expect("shutdown");
}
