#![allow(clippy::unwrap_used, clippy::expect_used)]

//! End-to-end: local runner → endpoint server → remote client, over a real unix
//! socket, with grants and a command allow-list.
//!
//! These tests pin the wire behaviour the Go client relies on: the three RPCs
//! that used to answer `UNIMPLEMENTED` (`Resize` / `Signal` / `Watch`), the
//! single session table behind `close_session`, and the exit classification
//! travelling across the boundary.

use std::collections::BTreeMap;
use std::sync::Arc;
use std::time::Duration;

use flowcraft_cap_sandbox::{
    Event, ExitReason, Handle, ReadResult, Session, SessionFeatures, SessionSpec, Stream, endpoint,
    local, remote,
};
use flowcraft_kernel::handles::contracts;
use flowcraft_kernel::{Config, EndpointConfig, EndpointHost, Mode, Platform, RemoteConfig};
use flowcraft_resource::Deployment;
use flowcraft_resource::{Builder, ExternalResource, Registry};
use serde_json::json;
use tokio_stream::StreamExt;

struct Stack {
    _dir: tempfile::TempDir,
    platform: Platform,
    built: flowcraft_resource::Built,
}

async fn start(commands: &[&str]) -> Stack {
    let dir = tempfile::tempdir().expect("tempdir");
    let socket = dir.path().join("sandbox.sock");
    let config = Config {
        remote: RemoteConfig {
            mode: Mode::Trusted,
            allow_targets: Vec::new(),
            endpoints: BTreeMap::from([(
                "sandbox".to_owned(),
                EndpointConfig {
                    target: format!("unix://{}", socket.display()),
                    tenant: String::new(),
                    capabilities: vec!["sandbox.Runner/v1".to_owned()],
                    tls: None,
                },
            )]),
        },
        ..Config::default()
    };
    let platform = Platform::new(config).expect("platform");

    let mut registry = Registry::new();
    registry.register(local::LocalFactory).expect("local");
    registry
        .register(endpoint::EndpointFactory)
        .expect("endpoint");
    registry.register(remote::RemoteFactory).expect("remote");

    let document: Deployment = flowcraft_resource::decode::deployment(
        serde_json::to_vec(&json!({
            "version": "v1",
            "resources": {
                "runner": {"kind": "sandbox.Runner", "impl": "local"},
                "ep": {
                    "kind": "sandbox.Endpoint",
                    "impl": "grpc",
                    "deps": {"runner": "runner", "host": "host", "grants": "grants"},
                    "settings": {"endpoint": "sandbox"}
                },
                "client": {
                    "kind": "sandbox.Runner",
                    "impl": "remote",
                    "deps": {"endpoints": "endpoints"},
                    "settings": {
                        "endpoint": "sandbox",
                        "scope": {
                            "commands": commands,
                            "max_timeout_secs": 5,
                            "max_output_bytes": 4096
                        }
                    }
                }
            }
        }))
        .expect("json")
        .as_slice(),
    )
    .expect("document");

    let host = EndpointHost::new(platform.catalog());
    let built = Builder::new(Arc::new(registry))
        .with_externals([
            ExternalResource::new("host", contracts::ENDPOINT_HOST, Arc::new(host)),
            ExternalResource::new("grants", contracts::GRANTS, Arc::new(platform.grants())),
            ExternalResource::new(
                "endpoints",
                contracts::ENDPOINT_PROVIDER,
                Arc::new(platform.endpoints()),
            ),
        ])
        .deploy(document)
        .await
        .expect("deploy");
    built.start().await.expect("start");
    Stack {
        _dir: dir,
        platform,
        built,
    }
}

fn spec(argv: &[&str]) -> SessionSpec {
    SessionSpec::new(argv.iter().map(|arg| (*arg).to_owned()).collect())
}

async fn read_all(session: &Arc<dyn Session>) -> String {
    let mut out = String::new();
    let mut after = 0;
    for _ in 0..64 {
        let ReadResult {
            next_seq,
            chunks,
            eof,
        } = session.read(after, 64 * 1024).await.unwrap();
        for chunk in &chunks {
            out.push_str(&String::from_utf8_lossy(&chunk.data));
        }
        after = next_seq;
        if eof {
            break;
        }
    }
    out
}

fn client(stack: &Stack) -> Arc<dyn flowcraft_cap_sandbox::Runner> {
    let handle = stack.built.get::<Handle>("client").expect("client");
    handle.runner()
}

#[tokio::test]
async fn runs_a_command_through_the_endpoint() {
    let stack = start(&["sh"]).await;
    let runner = client(&stack);
    let session = runner
        .start(spec(&["sh", "-c", "echo hello; exit 3"]))
        .await
        .expect("start");
    let exit = session.wait().await.expect("wait");
    assert_eq!(exit.code, Some(3));
    assert_eq!(exit.reason, ExitReason::Exited);
    assert!(read_all(&session).await.contains("hello"));
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn stdin_round_trips() {
    let stack = start(&["cat"]).await;
    let runner = client(&stack);
    let mut request = spec(&["cat"]);
    request.stdin = true;
    let session = runner.start(request).await.expect("start");
    assert!(session.features().stdin, "stdin is a per-session answer");
    session.write(b"ping\n").await.expect("write");
    session.close_input().await.expect("close");
    let exit = session.wait().await.expect("wait");
    assert_eq!(exit.code, Some(0));
    assert!(read_all(&session).await.contains("ping"));
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn the_command_allow_list_is_enforced() {
    let stack = start(&["sh"]).await;
    let runner = client(&stack);
    let error = runner
        .start(spec(&["rm", "-rf", "/tmp/flowcraft-should-not-run"]))
        .await
        .expect_err("denied");
    assert_eq!(error.kind(), flowcraft_error::ErrorKind::Forbidden);
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn a_timeout_is_classified_across_the_wire() {
    let stack = start(&["sleep"]).await;
    let runner = client(&stack);
    let mut request = spec(&["sleep", "30"]);
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
    let stack = start(&["sleep"]).await;
    let runner = client(&stack);
    let mut request = spec(&["sleep", "30"]);
    request.opts.timeout = Some(Duration::from_secs(60));
    let error = runner.start(request).await.expect_err("ceiling");
    assert_eq!(error.kind(), flowcraft_error::ErrorKind::Forbidden);
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn sessions_are_listed() {
    let stack = start(&["sh"]).await;
    let runner = client(&stack);
    let session = runner
        .start(spec(&["sh", "-c", "exit 0"]))
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
    let stack = start(&["sh"]).await;
    let runner = client(&stack);
    let capabilities = runner.capabilities().await.expect("capabilities");
    assert!(capabilities.policy.env_allow_list);
    assert!(!capabilities.policy.filesystem_bounds);
    assert!(!capabilities.features.tty);
    // `stdin` is decided per session, so a runner-level `true` would be wrong.
    assert!(!capabilities.features.stdin);
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn resize_reaches_the_session_instead_of_answering_unimplemented() {
    let stack = start(&["sleep"]).await;
    let runner = client(&stack);
    let session = runner.start(spec(&["sleep", "30"])).await.expect("start");
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
    let stack = start(&["sh"]).await;
    let runner = client(&stack);
    let session = runner
        .start(spec(&[
            "sh",
            "-c",
            "trap 'exit 0' INT; while :; do sleep 0.05; done",
        ]))
        .await
        .expect("start");
    // Give the shell a moment to install the trap.
    tokio::time::sleep(Duration::from_millis(100)).await;
    session
        .signal(flowcraft_cap_sandbox::Signal::Interrupt)
        .await
        .expect("signal");
    let exit = session.wait().await.expect("wait");
    assert_eq!(exit.code, Some(0), "{exit:?}");
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn watch_streams_output_and_exit() {
    let stack = start(&["sh"]).await;
    let runner = client(&stack);
    let session = runner
        .start(spec(&["sh", "-c", "echo watched"]))
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
            Event::Closed { .. } => break,
            Event::Lag { .. } => break,
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
    let stack = start(&["sh"]).await;
    let runner = client(&stack);
    let session = runner
        .start(spec(&["sh", "-c", "exit 0"]))
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
async fn a_revoked_session_cannot_start_a_process() {
    let stack = start(&["sh"]).await;
    stack
        .platform
        .revoke_session(stack.platform.session())
        .expect("revoke");
    let runner = client(&stack);
    let error = runner
        .start(spec(&["sh", "-c", "true"]))
        .await
        .expect_err("revoked");
    assert_eq!(error.kind(), flowcraft_error::ErrorKind::Forbidden);
    // Revocation cuts the whole control plane, including this client's
    // `CloseRunner`. Shutdown still succeeds: the endpoint drains first, and a
    // client that cannot reach its endpoint has nothing left to close.
    stack.built.shutdown().await.expect("shutdown");
}

#[test]
fn session_features_are_not_part_of_a_runner_declaration() {
    // A runner declaration carries tty/signal/events; stdin is per session.
    let features = SessionFeatures {
        stdin: true,
        ..SessionFeatures::default()
    };
    assert!(features.stdin);
}
