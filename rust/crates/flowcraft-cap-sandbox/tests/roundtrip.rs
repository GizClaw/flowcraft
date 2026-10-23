#![allow(clippy::unwrap_used, clippy::expect_used)]

//! End-to-end: local runner → endpoint server → remote client, over a real unix
//! socket, with grants and a command allow-list.

use std::collections::BTreeMap;
use std::sync::Arc;
use std::time::Duration;

use flowcraft_cap_sandbox::{
    ExecOptions, ExitStatus, Handle, ReadResult, Session, SessionSpec, endpoint, local, remote,
};
use flowcraft_kernel::handles::contracts;
use flowcraft_kernel::{Config, EndpointConfig, EndpointHost, Mode, Platform, RemoteConfig};
use flowcraft_resource::Deployment;
use flowcraft_resource::{Builder, ExternalResource, Registry};
use serde_json::json;

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
    SessionSpec {
        id: String::new(),
        argv: argv.iter().map(|arg| (*arg).to_owned()).collect(),
        tty: false,
        stdin: false,
        opts: ExecOptions::default(),
    }
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
        if eof || chunks.is_empty() {
            break;
        }
    }
    out
}

#[tokio::test]
async fn runs_a_command_through_the_endpoint() {
    let stack = start(&["sh"]).await;
    let runner = stack.built.get::<Handle>("client").expect("client");
    let session = runner
        .start(spec(&["sh", "-c", "echo hello; exit 3"]))
        .await
        .expect("start");
    let exit: ExitStatus = session.wait().await.expect("wait");
    assert_eq!(exit.code, Some(3));
    assert_eq!(exit.reason, "exited");
    assert!(read_all(&session).await.contains("hello"));
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn stdin_round_trips() {
    let stack = start(&["cat"]).await;
    let runner = stack.built.get::<Handle>("client").expect("client");
    let mut request = spec(&["cat"]);
    request.stdin = true;
    let session = runner.start(request).await.expect("start");
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
    let runner = stack.built.get::<Handle>("client").expect("client");
    let error = runner
        .start(spec(&["rm", "-rf", "/tmp/flowcraft-should-not-run"]))
        .await
        .expect_err("denied");
    assert_eq!(error.kind(), flowcraft_error::ErrorKind::Forbidden);
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn a_timeout_ends_the_session() {
    let stack = start(&["sleep"]).await;
    let runner = stack.built.get::<Handle>("client").expect("client");
    let mut request = spec(&["sleep", "30"]);
    request.opts.timeout = Some(Duration::from_millis(100));
    let session = runner.start(request).await.expect("start");
    let exit = session.wait().await.expect("wait");
    assert_eq!(exit.reason, "timeout");
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn a_timeout_above_the_grant_ceiling_is_refused() {
    let stack = start(&["sleep"]).await;
    let runner = stack.built.get::<Handle>("client").expect("client");
    let mut request = spec(&["sleep", "30"]);
    request.opts.timeout = Some(Duration::from_secs(60));
    let error = runner.start(request).await.expect_err("ceiling");
    assert_eq!(error.kind(), flowcraft_error::ErrorKind::Forbidden);
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn sessions_are_listed() {
    let stack = start(&["sh"]).await;
    let runner = stack.built.get::<Handle>("client").expect("client");
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
    let runner = stack.built.get::<Handle>("client").expect("client");
    let capabilities = runner.capabilities().await.expect("capabilities");
    assert!(capabilities.policy.env_allow_list);
    assert!(!capabilities.policy.filesystem_bounds);
    assert!(!capabilities.features.tty);
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn a_revoked_session_cannot_start_a_process() {
    let stack = start(&["sh"]).await;
    stack
        .platform
        .revoke_session(stack.platform.session())
        .expect("revoke");
    let runner = stack.built.get::<Handle>("client").expect("client");
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
