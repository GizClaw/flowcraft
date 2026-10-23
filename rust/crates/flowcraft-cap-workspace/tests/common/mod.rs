#![allow(dead_code)]
#![allow(clippy::unwrap_used, clippy::expect_used)]

//! A whole workspace stack for the integration tests.
//!
//! The document is the point of the fixture: every expression of the contract is
//! one resource, wired the way a deployment would wire it.
//!
//! ```text
//! local   workspace.Workspace/local    a directory
//! scoped  workspace.Workspace/scoped   + glob policy over `local`
//! view    workspace.Workspace/sub      + prefix "run-1" over `scoped`
//! endpoint workspace.Endpoint/grpc      serves `view` over a unix socket
//! remote  workspace.Workspace/remote    the endpoint, as the same contract
//! ```

use std::collections::BTreeMap;
use std::sync::Arc;

use flowcraft_cap_workspace::{Handle, endpoint, local, remote, scoped, sub};
use flowcraft_kernel::{Config, EndpointConfig, EndpointHost, Mode, Platform, RemoteConfig};
use flowcraft_resource::decode::deployment;
use flowcraft_resource::{Builder, Deployment, ExternalResource, Registry};
use serde_json::json;

/// Everything a test needs from one running stack.
pub struct Stack {
    pub _dir: tempfile::TempDir,
    pub platform: Platform,
    pub built: flowcraft_resource::Built,
}

impl Stack {
    /// The handle produced by one resource in the document.
    pub fn handle(&self, name: &str) -> Handle {
        let handle = self.built.get::<Handle>(name).expect("workspace handle");
        Handle::new(handle.workspace())
    }
}

/// Build, start, and return the stack. Call `shutdown` on the built graph when
/// the test is done.
pub async fn start() -> Stack {
    let dir = tempfile::tempdir().expect("tempdir");
    let socket = dir.path().join("ws.sock");
    let config = Config {
        remote: RemoteConfig {
            mode: Mode::Trusted,
            allow_targets: Vec::new(),
            endpoints: BTreeMap::from([(
                "ws".to_owned(),
                EndpointConfig {
                    target: format!("unix://{}", socket.display()),
                    tenant: String::new(),
                    capabilities: vec!["workspace.Workspace/v1".to_owned()],
                    tls: None,
                },
            )]),
        },
        ..Config::default()
    };
    let platform = Platform::new(config).expect("platform");

    let mut registry = Registry::new();
    registry.register(local::LocalFactory).expect("local");
    registry.register(scoped::ScopedFactory).expect("scoped");
    registry.register(sub::SubFactory).expect("sub");
    registry
        .register(endpoint::EndpointFactory)
        .expect("endpoint");
    registry.register(remote::RemoteFactory).expect("remote");

    let document: Deployment = deployment(
        serde_json::to_vec(&json!({
            "version": "v1",
            "resources": {
                "local": {
                    "kind": "workspace.Workspace",
                    "impl": "local",
                    "settings": {"root": dir.path().join("data")}
                },
                "scoped": {
                    "kind": "workspace.Workspace",
                    "impl": "scoped",
                    "deps": {"workspace": "local"},
                    "settings": {
                        "deny_read": ["secrets/**"],
                        "allow_write": ["**"],
                        "mandatory_deny": ["**/.env"]
                    }
                },
                "view": {
                    "kind": "workspace.Workspace",
                    "impl": "sub",
                    "deps": {"workspace": "scoped"},
                    "settings": {"prefix": "run-1"}
                },
                "endpoint": {
                    "kind": "workspace.Endpoint",
                    "impl": "grpc",
                    "deps": {"workspace": "view", "host": "host", "grants": "grants"},
                    "settings": {"endpoint": "ws"}
                },
                "remote": {
                    "kind": "workspace.Workspace",
                    "impl": "remote",
                    "deps": {"endpoints": "endpoints"},
                    "settings": {"endpoint": "ws", "scope": {"write": true}}
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
            ExternalResource::new(
                "host",
                flowcraft_kernel::handles::contracts::ENDPOINT_HOST,
                Arc::new(host),
            ),
            ExternalResource::new(
                "grants",
                flowcraft_kernel::handles::contracts::GRANTS,
                Arc::new(platform.grants()),
            ),
            ExternalResource::new(
                "endpoints",
                flowcraft_kernel::handles::contracts::ENDPOINT_PROVIDER,
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
