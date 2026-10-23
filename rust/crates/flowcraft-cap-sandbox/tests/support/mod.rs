//! Shared test scaffolding: a real endpoint over a unix socket, a remote client
//! behind a grant, and the small helpers both integration suites need.

#![allow(dead_code, clippy::unwrap_used, clippy::expect_used)]

use std::collections::BTreeMap;
use std::sync::Arc;

use flowcraft_cap_sandbox::{
    Handle, ReadResult, Runner, Session, SessionSpec, endpoint, local, remote,
};
use flowcraft_kernel::handles::contracts;
use flowcraft_kernel::{Config, EndpointConfig, EndpointHost, Mode, Platform, RemoteConfig};
use flowcraft_resource::Deployment;
use flowcraft_resource::{Builder, ExternalResource, Registry};
use serde_json::json;

/// A started deployment: the endpoint is listening and the client is built.
pub struct Stack {
    /// Also the local runner's root, so it outlives the deployment.
    pub dir: tempfile::TempDir,
    pub platform: Platform,
    pub built: flowcraft_resource::Built,
}

/// Deploy `local → endpoint → remote` for the given command allow-list, with the
/// local runner rooted at a fresh temporary directory.
pub async fn stack(commands: &[&str]) -> Stack {
    let dir = tempfile::tempdir().expect("tempdir");
    let socket = dir.path().join("sandbox.sock");
    let root = dir.path().join("root");
    std::fs::create_dir(&root).expect("root");
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
                "runner": {
                    "kind": "sandbox.Runner",
                    "impl": "local",
                    "settings": {"root": root.display().to_string()}
                },
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
                            "max_output_bytes": 1 << 20
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
        dir,
        platform,
        built,
    }
}

/// The remote runner of a started stack.
pub fn remote_runner(stack: &Stack) -> Arc<dyn Runner> {
    stack
        .built
        .get::<Handle>("client")
        .expect("client")
        .runner()
}

/// A non-interactive spec for `argv`.
pub fn spec(argv: &[&str]) -> SessionSpec {
    SessionSpec::new(argv.iter().map(|arg| (*arg).to_owned()).collect())
}

/// Read until EOF.
pub async fn read_all(session: &Arc<dyn Session>) -> String {
    read_all_from(session, 0).await.1
}

/// Read from `after` until EOF, returning the final cursor and the text.
pub async fn read_all_from(session: &Arc<dyn Session>, after: u64) -> (u64, String) {
    let mut out = String::new();
    let mut cursor = after;
    for _ in 0..256 {
        let ReadResult {
            next_seq,
            chunks,
            eof,
        } = session.read(cursor, 64 * 1024).await.expect("read");
        for chunk in &chunks {
            out.push_str(&String::from_utf8_lossy(&chunk.data));
        }
        cursor = next_seq;
        if eof {
            break;
        }
    }
    (cursor, out)
}
