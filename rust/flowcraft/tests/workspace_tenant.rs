// A test may reach the filesystem directly: it has no executor to starve.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::disallowed_methods)]

//! Tenant-scoped endpoints: a grant carries the caller's tenant, and the
//! platform refuses to mint one for the wrong tenant.

use std::collections::BTreeMap;
use std::sync::Arc;

use flowcraft::caps::workspace::{Handle, endpoint, local, remote};
use flowcraft::kernel::handles::contracts;
use flowcraft::kernel::{Config, EndpointConfig, EndpointHost, Mode, Platform, RemoteConfig};
use flowcraft::resource::Deployment;
use flowcraft::resource::{Builder, ExternalResource, Registry};
use serde_json::json;

fn platform(dir: &std::path::Path) -> Platform {
    let config = Config {
        remote: RemoteConfig {
            mode: Mode::Trusted,
            allow_targets: Vec::new(),
            endpoints: BTreeMap::from([(
                "ws".to_owned(),
                EndpointConfig {
                    target: format!("unix://{}", dir.join("ws.sock").display()),
                    tenant: "a".to_owned(),
                    capabilities: vec!["workspace.Workspace/v1".to_owned()],
                    tls: None,
                },
            )]),
        },
        ..Config::default()
    };
    Platform::new(config).expect("platform")
}

fn registry() -> Registry {
    let mut registry = Registry::new();
    registry.register(local::LocalFactory).expect("local");
    registry
        .register(endpoint::EndpointFactory)
        .expect("endpoint");
    registry.register(remote::RemoteFactory).expect("remote");
    registry
}

fn document(dir: &std::path::Path, tenant: Option<&str>) -> Deployment {
    let mut settings = json!({"endpoint": "ws", "scope": {"write": true}});
    if let Some(tenant) = tenant {
        settings["tenant"] = json!(tenant);
    }
    flowcraft::resource::decode::deployment(
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
                    "deps": {"workspace": "local", "host": "host", "grants": "grants"},
                    "settings": {"endpoint": "ws"}
                },
                "client": {
                    "kind": "workspace.Workspace",
                    "impl": "remote",
                    "deps": {"endpoints": "endpoints"},
                    "settings": settings
                }
            }
        }))
        .expect("json")
        .as_slice(),
    )
    .expect("document")
}

async fn deploy(
    platform: &Platform,
    dir: &std::path::Path,
    tenant: Option<&str>,
) -> flowcraft::error::Result<flowcraft::resource::Built> {
    let host = EndpointHost::new(platform.catalog());
    Builder::new(Arc::new(registry()))
        .with_externals([
            ExternalResource::new("host", contracts::ENDPOINT_HOST, Arc::new(host)),
            ExternalResource::new("grants", contracts::GRANTS, Arc::new(platform.grants())),
            ExternalResource::new(
                "endpoints",
                contracts::ENDPOINT_PROVIDER,
                Arc::new(platform.endpoints()),
            ),
        ])
        .deploy(document(dir, tenant))
        .await
}

#[tokio::test]
async fn a_grant_carries_the_configured_tenant() {
    let dir = tempfile::tempdir().expect("tempdir");
    let platform = platform(dir.path());
    let built = deploy(&platform, dir.path(), Some("a"))
        .await
        .expect("deploy");
    built.start().await.expect("start");
    let client = built.get::<Handle>("client").expect("client");
    client.write("tenanted.txt", b"ok").await.expect("write");
    assert_eq!(client.read("tenanted.txt").await.expect("read"), b"ok");
    built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn an_endpoint_with_a_tenant_refuses_a_tenantless_grant() {
    let dir = tempfile::tempdir().expect("tempdir");
    let platform = platform(dir.path());
    let error = deploy(&platform, dir.path(), None)
        .await
        .expect_err("tenant mismatch");
    assert_eq!(error.kind(), flowcraft::error::ErrorKind::Forbidden);
    assert!(error.to_string().contains("\"a\""), "{error}");
}
