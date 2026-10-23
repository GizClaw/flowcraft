#![allow(clippy::unwrap_used, clippy::expect_used)]

//! Reload semantics: a new generation swaps in, the old one drains, and a bad
//! document costs nothing.

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::sync::Arc;

use flowcraft_cap_workspace::Handle;
use flowcraft_kernel::{Config, EndpointConfig, Mode, Platform, RemoteConfig};
use flowcraft_platformd::supervisor::Supervisor;
use serde_json::json;

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

fn document(root: &Path) -> Vec<u8> {
    serde_json::to_vec(&json!({
        "version": "v1",
        "resources": {
            "local": {
                "kind": "workspace.Workspace",
                "impl": "local",
                "settings": {"root": root}
            },
            "ep": {
                "kind": "workspace.Endpoint",
                "impl": "grpc",
                "deps": {"workspace": "local"},
                "settings": {"endpoint": "ws"}
            },
            "client": {
                "kind": "workspace.Workspace",
                "impl": "remote",
                "deps": {"endpoints": "endpoints"},
                "settings": {"endpoint": "ws", "scope": {"write": true}}
            }
        }
    }))
    .expect("json")
}

struct Fixture {
    _dir: tempfile::TempDir,
    deploy: PathBuf,
    first_root: PathBuf,
    second_root: PathBuf,
}

fn fixture() -> Fixture {
    let dir = tempfile::tempdir().expect("tempdir");
    let deploy = dir.path().join("deploy.json");
    let first_root = dir.path().join("data-a");
    let second_root = dir.path().join("data-b");
    std::fs::create_dir_all(&first_root).unwrap();
    std::fs::create_dir_all(&second_root).unwrap();
    std::fs::write(&deploy, document(&first_root)).unwrap();
    Fixture {
        _dir: dir,
        deploy,
        first_root,
        second_root,
    }
}

#[tokio::test]
async fn reload_swaps_generations_and_serves_from_the_new_one() {
    let fixture = fixture();
    let supervisor = Supervisor::boot(platform(fixture._dir.path()), vec![fixture.deploy.clone()])
        .await
        .expect("boot");
    assert_eq!(supervisor.generation().await, 1);

    let client = supervisor
        .resource::<Handle>("client")
        .await
        .expect("client");
    client.write("first.txt", b"a").await.expect("write");
    assert!(fixture.first_root.join("first.txt").exists());

    std::fs::write(&fixture.deploy, document(&fixture.second_root)).unwrap();
    let outcome = supervisor.reload().await.expect("reload");
    assert_eq!(outcome.previous, 1);
    assert_eq!(outcome.current, 2);
    assert_eq!(outcome.resources, 3);
    assert_eq!(supervisor.generation().await, 2);

    // The new generation's endpoint is bound to the same socket, so the new
    // client reaches the new local store.
    let client = supervisor
        .resource::<Handle>("client")
        .await
        .expect("client");
    client.write("second.txt", b"b").await.expect("write");
    assert!(fixture.second_root.join("second.txt").exists());
    assert!(!fixture.first_root.join("second.txt").exists());

    supervisor.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn a_broken_document_keeps_the_running_generation() {
    let fixture = fixture();
    let supervisor = Supervisor::boot(platform(fixture._dir.path()), vec![fixture.deploy.clone()])
        .await
        .expect("boot");

    std::fs::write(&fixture.deploy, b"{ not json").unwrap();
    let error = supervisor.reload().await.expect_err("broken document");
    assert_eq!(error.kind(), flowcraft_error::ErrorKind::Validation);
    assert_eq!(supervisor.generation().await, 1);

    // The old generation still serves: its endpoint was never touched.
    let client = supervisor
        .resource::<Handle>("client")
        .await
        .expect("client");
    client.write("still-here.txt", b"x").await.expect("write");
    assert!(fixture.first_root.join("still-here.txt").exists());

    supervisor.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn a_plan_error_keeps_the_running_generation() {
    let fixture = fixture();
    let supervisor = Supervisor::boot(platform(fixture._dir.path()), vec![fixture.deploy.clone()])
        .await
        .expect("boot");

    // Parseable, but the kind does not exist: plan fails before anything starts.
    std::fs::write(
        &fixture.deploy,
        serde_json::to_vec(&json!({
            "version": "v1",
            "resources": {"nope": {"kind": "no.Such", "impl": "x"}}
        }))
        .unwrap(),
    )
    .unwrap();
    let error = supervisor.reload().await.expect_err("plan error");
    assert!(error.to_string().contains("no.Such"), "{error}");
    assert_eq!(supervisor.generation().await, 1);
    supervisor.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn generation_ids_are_monotonic() {
    let fixture = fixture();
    let supervisor = Supervisor::boot(platform(fixture._dir.path()), vec![fixture.deploy.clone()])
        .await
        .expect("boot");
    for expected in 2..=4 {
        let outcome = supervisor.reload().await.expect("reload");
        assert_eq!(outcome.current, expected);
        assert_eq!(supervisor.generation().await, expected);
    }
    supervisor.shutdown().await.expect("shutdown");
    let _: Arc<Supervisor> = supervisor;
}

#[tokio::test]
async fn later_layers_override_earlier_ones() {
    use flowcraft_cap_workspace::Handle;

    let fixture = fixture();
    let base = fixture.deploy.clone();
    let overlay = fixture._dir.path().join("overlay.json");
    std::fs::write(
        &overlay,
        serde_json::to_vec(&json!({
            "resources": {
                "local": {
                    "kind": "workspace.Workspace",
                    "impl": "local",
                    "settings": {"root": fixture.second_root}
                }
            },
            "entries": ["local"]
        }))
        .unwrap(),
    )
    .expect("overlay");

    let supervisor = Supervisor::boot(platform(fixture._dir.path()), vec![base, overlay])
        .await
        .expect("boot");
    let local = supervisor.resource::<Handle>("local").await.expect("local");
    local.write("layered.txt", b"x").await.expect("write");
    assert!(fixture.second_root.join("layered.txt").exists());
    assert!(!fixture.first_root.join("layered.txt").exists());
    assert_eq!(supervisor.entries().await, vec!["local".to_owned()]);
    supervisor.shutdown().await.expect("shutdown");
}
