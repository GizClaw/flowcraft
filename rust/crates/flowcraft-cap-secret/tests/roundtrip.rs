#![allow(clippy::unwrap_used, clippy::expect_used)]

//! End-to-end: memory provider → endpoint server → remote client, over a real
//! unix socket, with grants and a scope prefix.

use std::collections::BTreeMap;
use std::sync::Arc;

use flowcraft_cap_secret::{Handle, endpoint, memory, remote};
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
    let socket = dir.path().join("secrets.sock");
    let config = Config {
        remote: RemoteConfig {
            mode: Mode::Trusted,
            allow_targets: Vec::new(),
            endpoints: BTreeMap::from([(
                "secrets".to_owned(),
                EndpointConfig {
                    target: format!("unix://{}", socket.display()),
                    tenant: String::new(),
                    capabilities: vec!["secret.Store/v1".to_owned()],
                    tls: None,
                },
            )]),
        },
        ..Config::default()
    };
    let platform = Platform::new(config).expect("platform");

    let mut registry = Registry::new();
    registry.register(memory::MemoryFactory).expect("memory");
    registry
        .register(endpoint::EndpointFactory)
        .expect("endpoint");
    registry.register(remote::RemoteFactory).expect("remote");

    let document: Deployment = flowcraft_resource::decode::deployment(
        serde_json::to_vec(&json!({
            "version": "v1",
            "resources": {
                "store": {
                    "kind": "secret.Store",
                    "impl": "memory",
                    "settings": {"values": {"team/token": "s3cr3t"}}
                },
                "ep": {
                    "kind": "secret.Endpoint",
                    "impl": "grpc",
                    "deps": {"store": "store", "host": "host", "grants": "grants"},
                    "settings": {"endpoint": "secrets"}
                },
                "client": {
                    "kind": "secret.Store",
                    "impl": "remote",
                    "deps": {"endpoints": "endpoints"},
                    "settings": {"endpoint": "secrets", "scope": {"prefix": prefix}}
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

#[tokio::test]
async fn remote_lookup_reads_through_the_endpoint() {
    let stack = start("team/").await;
    let client = stack.built.get::<Handle>("client").expect("client");
    assert_eq!(
        client.lookup("team/token").await.expect("lookup"),
        Some("s3cr3t".to_owned())
    );
    assert_eq!(client.lookup("team/missing").await.expect("missing"), None);
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn the_local_store_and_the_remote_client_agree() {
    let stack = start("team/").await;
    let local = stack.built.get::<Handle>("store").expect("store");
    let client = stack.built.get::<Handle>("client").expect("client");
    assert_eq!(
        local.lookup("team/token").await.expect("local"),
        client.lookup("team/token").await.expect("remote")
    );
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn the_scope_prefix_confines_names() {
    let stack = start("team/").await;
    let client = stack.built.get::<Handle>("client").expect("client");
    let error = client
        .lookup("other/token")
        .await
        .expect_err("out of scope");
    assert_eq!(error.kind(), flowcraft_error::ErrorKind::Forbidden);
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn a_revoked_session_cannot_read() {
    let stack = start("team/").await;
    stack
        .platform
        .revoke_session(stack.platform.session())
        .expect("revoke");
    let client = stack.built.get::<Handle>("client").expect("client");
    let error = client.lookup("team/token").await.expect_err("revoked");
    assert_eq!(error.kind(), flowcraft_error::ErrorKind::Forbidden);
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn an_endpoint_only_serves_its_own_capability() {
    let stack = start("").await;
    let error = stack
        .platform
        .grant("secrets", "workspace.Workspace/v1", "s-1", "", json!({}))
        .expect_err("capability");
    assert_eq!(error.kind(), flowcraft_error::ErrorKind::NotAvailable);
    stack.built.shutdown().await.expect("shutdown");
}
