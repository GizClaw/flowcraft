#![allow(clippy::unwrap_used, clippy::expect_used)]

//! End-to-end: local directory provider → endpoint server → remote client,
//! over a real unix socket, with grants and an execution-id prefix.

use std::collections::BTreeMap;
use std::sync::Arc;

use flowcraft_cap_checkpoint::{Handle, endpoint, local, remote};
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

async fn start(prefix: &str) -> Stack {
    let dir = tempfile::tempdir().expect("tempdir");
    let socket = dir.path().join("checkpoints.sock");
    let config = Config {
        remote: RemoteConfig {
            mode: Mode::Trusted,
            allow_targets: Vec::new(),
            endpoints: BTreeMap::from([(
                "checkpoints".to_owned(),
                EndpointConfig {
                    target: format!("unix://{}", socket.display()),
                    tenant: String::new(),
                    capabilities: vec!["checkpoint.Store/v1".to_owned()],
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

    let root = dir.path().join("store").display().to_string();
    let document: Deployment = flowcraft_resource::decode::deployment(
        serde_json::to_vec(&json!({
            "version": "v1",
            "resources": {
                "store": {
                    "kind": "checkpoint.Store",
                    "impl": "local",
                    "settings": {"root": root}
                },
                "ep": {
                    "kind": "checkpoint.Endpoint",
                    "impl": "grpc",
                    "deps": {"store": "store", "host": "host", "grants": "grants"},
                    "settings": {"endpoint": "checkpoints"}
                },
                "client": {
                    "kind": "checkpoint.Store",
                    "impl": "remote",
                    "deps": {"endpoints": "endpoints"},
                    "settings": {"endpoint": "checkpoints", "scope": {"prefix": prefix}}
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

fn payload(exec_id: &str) -> Vec<u8> {
    serde_json::to_vec(&json!({"exec_id": exec_id, "board": {"messages": []}})).expect("payload")
}

#[tokio::test]
async fn save_load_list_delete_round_trip() {
    let stack = start("team-a/").await;
    let client = stack.built.get::<Handle>("client").expect("client");
    let bytes = payload("team-a/run-1");
    client.save("team-a/run-1", &bytes).await.expect("save");
    assert_eq!(
        client.load("team-a/run-1").await.expect("load"),
        Some(bytes.clone())
    );
    assert_eq!(
        client.list().await.expect("list"),
        vec!["team-a/run-1".to_owned()]
    );
    client.delete("team-a/run-1").await.expect("delete");
    assert_eq!(client.load("team-a/run-1").await.expect("load"), None);
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn the_local_store_sees_what_the_remote_client_wrote() {
    let stack = start("team-a/").await;
    let local = stack.built.get::<Handle>("store").expect("store");
    let client = stack.built.get::<Handle>("client").expect("client");
    let bytes = payload("team-a/run-9");
    client.save("team-a/run-9", &bytes).await.expect("save");
    // The bytes are stored untouched: the envelope survives the round trip.
    assert_eq!(
        local.load("team-a/run-9").await.expect("local"),
        Some(bytes)
    );
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn the_scope_prefix_confines_ids_and_listing() {
    let stack = start("team-a/").await;
    let client = stack.built.get::<Handle>("client").expect("client");
    let error = client
        .save("team-b/run-1", &payload("team-b/run-1"))
        .await
        .expect_err("out of scope");
    assert_eq!(error.kind(), flowcraft_error::ErrorKind::Forbidden);
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn a_payload_for_another_id_is_refused_locally() {
    let stack = start("team-a/").await;
    let client = stack.built.get::<Handle>("client").expect("client");
    let error = client
        .save("team-a/run-1", &payload("team-a/run-2"))
        .await
        .expect_err("mismatch");
    assert_eq!(error.kind(), flowcraft_error::ErrorKind::Validation);
    assert!(error.to_string().contains("run-2"), "{error}");
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn a_revoked_session_cannot_read() {
    let stack = start("team-a/").await;
    stack
        .platform
        .revoke_session(stack.platform.session())
        .expect("revoke");
    let client = stack.built.get::<Handle>("client").expect("client");
    let error = client.load("team-a/run-1").await.expect_err("revoked");
    assert_eq!(error.kind(), flowcraft_error::ErrorKind::Forbidden);
    stack.built.shutdown().await.expect("shutdown");
}
