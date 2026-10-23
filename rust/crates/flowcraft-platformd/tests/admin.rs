#![allow(clippy::unwrap_used, clippy::expect_used)]

//! The admin socket: status, reload, drain, revoke, and the `ctl` client.

mod support;

use std::collections::BTreeMap;
use std::path::Path;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};

use flowcraft_kernel::{Config, EndpointConfig, Mode, Platform, RemoteConfig};
use flowcraft_platformd::admin::{AdminState, serve};
use flowcraft_platformd::supervisor::Supervisor;
use serde_json::{Value, json};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};

fn platform(dir: &Path) -> Platform {
    let config = Config {
        remote: RemoteConfig {
            mode: Mode::Trusted,
            allow_targets: Vec::new(),
            endpoints: BTreeMap::from([(
                "ws".to_owned(),
                EndpointConfig {
                    target: format!("unix://{}", dir.join("ws.sock").display()),
                    tenant: String::new(),
                    capabilities: vec!["workspace.Workspace/v1".to_owned()],
                    tls: None,
                },
            )]),
        },
        ..Config::default()
    };
    Platform::new(config).expect("platform")
}

fn document(dir: &Path) -> Vec<u8> {
    serde_json::to_vec(&json!({
        "version": "v1",
        "resources": {
            "local": {
                "kind": "workspace.Workspace",
                "impl": "local",
                "settings": {"root": dir.join("data")}
            },
            "ep": {
                "kind": "workspace.Endpoint",
                "impl": "grpc",
                "deps": {"workspace": "local"},
                "settings": {"endpoint": "ws"}
            }
        },
        "entries": ["local"]
    }))
    .expect("json")
}

async fn send(socket: &Path, request: Value) -> Value {
    let mut stream = tokio::net::UnixStream::connect(socket)
        .await
        .expect("connect admin");
    stream
        .write_all(format!("{request}\n").as_bytes())
        .await
        .expect("write");
    let mut line = String::new();
    BufReader::new(stream)
        .read_line(&mut line)
        .await
        .expect("read");
    serde_json::from_str(line.trim()).expect("json response")
}

struct Fixture {
    _dir: tempfile::TempDir,
    deploy: std::path::PathBuf,
    socket: std::path::PathBuf,
    _handle: flowcraft_platformd::admin::AdminHandle,
    ready: Arc<AtomicBool>,
}

async fn fixture() -> Fixture {
    let dir = tempfile::tempdir().expect("tempdir");
    let deploy = dir.path().join("deploy.json");
    std::fs::write(&deploy, document(dir.path())).expect("deploy");
    let supervisor = Supervisor::boot(platform(dir.path()), vec![deploy.clone()])
        .await
        .expect("boot");
    let socket = dir.path().join("admin.sock");
    let ready = Arc::new(AtomicBool::new(true));
    let handle = serve(
        &socket,
        AdminState::new(
            supervisor,
            vec!["demo".to_owned()],
            ready.clone(),
            1,
            Arc::new(flowcraft_platformd::logging::Applied::disabled()),
        ),
    )
    .await
    .expect("admin");
    Fixture {
        _dir: dir,
        deploy,
        socket,
        _handle: handle,
        ready,
    }
}

#[tokio::test]
async fn diag_reports_the_pipeline_without_its_secrets() {
    let fixture = fixture().await;
    let response = send(&fixture.socket, json!({"command": "diag"})).await;
    assert_eq!(response["ok"], true, "{response}");
    assert_eq!(response["service"], "flowcraftd");
    assert!(
        response["session"]
            .as_str()
            .is_some_and(|id| id.starts_with("s-"))
    );
    assert!(response.get("ring").is_some());
    // The identity salt and any grant token must never travel over this socket.
    let text = response.to_string();
    assert!(!text.contains("salt"), "{text}");
    assert!(!text.contains("token"), "{text}");
}

#[tokio::test]
async fn status_reload_drain_and_revoke() {
    let fixture = fixture().await;

    let status = send(&fixture.socket, json!({"command": "status"})).await;
    assert_eq!(status["ok"], true, "{status}");
    assert_eq!(status["generation"], 1);
    assert_eq!(status["ready"], true);
    assert_eq!(status["crafts"][0], "demo");
    assert_eq!(status["endpoints"], 1);
    assert_eq!(status["entries"][0], "local");

    let reloaded = send(&fixture.socket, json!({"command": "reload"})).await;
    assert_eq!(reloaded["ok"], true, "{reloaded}");
    assert_eq!(reloaded["previous"], 1);
    assert_eq!(reloaded["current"], 2);

    let status = send(&fixture.socket, json!({"command": "status"})).await;
    assert_eq!(status["generation"], 2);

    let drained = send(&fixture.socket, json!({"command": "drain"})).await;
    assert_eq!(drained["ready"], false);
    assert!(!fixture.ready.load(Ordering::SeqCst));

    let unknown = send(&fixture.socket, json!({"command": "nope"})).await;
    assert_eq!(unknown["ok"], false);
    assert!(
        unknown["error"]
            .as_str()
            .unwrap_or_default()
            .contains("nope"),
        "{unknown}"
    );

    let revoke = send(
        &fixture.socket,
        json!({"command": "revoke", "session": "bootstrap"}),
    )
    .await;
    assert_eq!(revoke["ok"], true, "{revoke}");

    // A reload after a plan error reports the error and keeps the generation.
    std::fs::write(&fixture.deploy, b"{ broken").expect("write");
    let failed = send(&fixture.socket, json!({"command": "reload"})).await;
    assert_eq!(failed["ok"], false);
    let status = send(&fixture.socket, json!({"command": "status"})).await;
    assert_eq!(status["generation"], 2);
}

#[tokio::test]
async fn the_ctl_binary_talks_to_the_socket() {
    let fixture = fixture().await;
    let output = support::isolated_async(tokio::process::Command::new(support::BIN))
        .args([
            "ctl",
            "--socket",
            fixture.socket.to_str().expect("path"),
            "status",
        ])
        .output()
        .await
        .expect("ctl");
    assert!(
        output.status.success(),
        "stderr: {}",
        String::from_utf8_lossy(&output.stderr)
    );
    let text = String::from_utf8_lossy(&output.stdout);
    assert!(text.contains("\"generation\": 1"), "{text}");

    let output = support::isolated_async(tokio::process::Command::new(support::BIN))
        .args([
            "ctl",
            "--socket",
            fixture.socket.to_str().expect("path"),
            "reload",
        ])
        .output()
        .await
        .expect("ctl reload");
    assert!(output.status.success());
    let text = String::from_utf8_lossy(&output.stdout);
    assert!(text.contains("\"current\": 2"), "{text}");

    let output = support::isolated_async(tokio::process::Command::new(support::BIN))
        .args([
            "ctl",
            "--socket",
            fixture.socket.to_str().expect("path"),
            "nope",
        ])
        .output()
        .await
        .expect("ctl nope");
    assert!(!output.status.success());
}
