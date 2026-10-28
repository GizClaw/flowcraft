// A test may reach the filesystem directly: it has no executor to starve.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::disallowed_methods)]

//! Rotation: a change under the bootstrap secret directory reloads the graph.

use std::collections::BTreeMap;
use std::path::Path;
use std::sync::Arc;
use std::sync::atomic::AtomicBool;
use std::time::Duration;

use flowcraft::kernel::{
    BootstrapConfig, Config, EndpointConfig, FileProvider, Mode, Platform, RemoteConfig,
};
use flowcraft_platformd::supervisor::Supervisor;
use flowcraft_platformd::watcher;

fn config(dir: &Path, secrets: &Path) -> Config {
    Config {
        bootstrap: BootstrapConfig {
            dir: secrets.display().to_string(),
            watch: Some(true),
            ..BootstrapConfig::default()
        },
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
    }
}

fn document(dir: &Path) -> Vec<u8> {
    serde_json::to_vec(&serde_json::json!({
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
        }
    }))
    .expect("json")
}

async fn boot(dir: &Path, secrets: &Path) -> Supervisor {
    let config = config(dir, secrets);
    config.validate().expect("config");
    let provider = Arc::new(FileProvider::new(secrets).expect("provider"));
    let platform = Platform::with_secrets(config, provider).expect("platform");
    let deploy = dir.join("deploy.json");
    std::fs::write(&deploy, document(dir)).expect("deploy");
    let supervisor = Supervisor::boot(platform, vec![deploy])
        .await
        .expect("boot");
    Arc::into_inner(supervisor).expect("sole owner")
}

#[tokio::test]
async fn a_secret_change_triggers_a_reload() {
    let dir = tempfile::tempdir().expect("tempdir");
    let secrets = dir.path().join("secrets");
    std::fs::create_dir_all(&secrets).expect("secrets");
    std::fs::write(secrets.join("ca.pem"), b"v1").expect("seed");

    let supervisor = Arc::new(boot(dir.path(), &secrets).await);
    let ready = Arc::new(AtomicBool::new(true));
    let handle = watcher::watch(&secrets, supervisor.clone(), ready.clone()).expect("watch");

    std::fs::write(secrets.join("ca.pem"), b"v2").expect("rotate");
    for _ in 0..60 {
        if supervisor.generation().await >= 2 {
            break;
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
    assert_eq!(supervisor.generation().await, 2);

    handle.stop().await;
    supervisor.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn changes_are_ignored_while_draining() {
    let dir = tempfile::tempdir().expect("tempdir");
    let secrets = dir.path().join("secrets");
    std::fs::create_dir_all(&secrets).expect("secrets");
    std::fs::write(secrets.join("ca.pem"), b"v1").expect("seed");

    let supervisor = Arc::new(boot(dir.path(), &secrets).await);
    let ready = Arc::new(AtomicBool::new(false));
    let handle = watcher::watch(&secrets, supervisor.clone(), ready.clone()).expect("watch");

    std::fs::write(secrets.join("ca.pem"), b"v2").expect("rotate");
    tokio::time::sleep(Duration::from_millis(1500)).await;
    assert_eq!(supervisor.generation().await, 1);

    handle.stop().await;
    supervisor.shutdown().await.expect("shutdown");
}
