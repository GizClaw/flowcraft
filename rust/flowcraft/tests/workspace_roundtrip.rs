// A test may reach the filesystem directly: it has no executor to starve.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::disallowed_methods)]

//! End-to-end: local provider → endpoint server → remote client, over a real
//! unix socket, with real grants and scope enforcement.

use std::collections::BTreeMap;
use std::sync::Arc;

use flowcraft::caps::workspace::{Handle, LocalWorkspace, local, remote};
use flowcraft::kernel::{Config, EndpointConfig, EndpointHost, Mode, Platform, RemoteConfig};
use flowcraft::resource::Deployment;
use flowcraft::resource::{Builder, ExternalResource, Registry};
use serde_json::json;

struct Stack {
    _dir: tempfile::TempDir,
    platform: Platform,
    built: flowcraft::resource::Built,
}

async fn start_stack(root_scope: &str, write: bool) -> Stack {
    let dir = tempfile::tempdir().expect("tempdir");
    let socket = dir.path().join("ws.sock");
    let target = format!("unix://{}", socket.display());

    let config = Config {
        remote: RemoteConfig {
            mode: Mode::Trusted,
            allow_targets: Vec::new(),
            endpoints: BTreeMap::from([(
                "ws".to_owned(),
                EndpointConfig {
                    target,
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
    registry
        .register(pointer_endpoint_factory())
        .expect("endpoint");
    registry.register(remote::RemoteFactory).expect("remote");

    let document: Deployment = flowcraft::resource::decode::deployment(
        serde_json::to_vec(&json!({
            "version": "v1",
            "resources": {
                "local": {
                    "kind": "workspace.Workspace",
                    "impl": "local",
                    "settings": {"root": dir.path().join("data")}
                },
                "ep": {
                    "kind": "workspace.Endpoint",
                    "impl": "grpc",
                    "deps": {"workspace": "local", "host": "host", "grants": "grants"},
                    "settings": {"endpoint": "ws"}
                },
                "remote": {
                    "kind": "workspace.Workspace",
                    "impl": "remote",
                    "deps": {"endpoints": "endpoints"},
                    "settings": {"endpoint": "ws", "scope": {"root": root_scope, "write": write}}
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
                flowcraft::kernel::handles::contracts::ENDPOINT_HOST,
                Arc::new(host),
            ),
            ExternalResource::new(
                "grants",
                flowcraft::kernel::handles::contracts::GRANTS,
                Arc::new(platform.grants()),
            ),
            ExternalResource::new(
                "endpoints",
                flowcraft::kernel::handles::contracts::ENDPOINT_PROVIDER,
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

/// The endpoint factory, re-exported for the test through the module path.
fn pointer_endpoint_factory()
-> impl flowcraft::resource::Factory<Output = impl flowcraft::resource::Resource> {
    flowcraft::caps::workspace::endpoint::EndpointFactory
}

#[tokio::test]
async fn remote_reads_and_writes_through_the_endpoint() {
    let stack = start_stack("", true).await;
    let remote = stack.built.get::<Handle>("remote").expect("remote handle");
    remote.write("notes/a.txt", b"hello").await.expect("write");
    assert_eq!(remote.read("notes/a.txt").await.expect("read"), b"hello");
    let entries = remote.list("notes").await.expect("list");
    assert_eq!(entries.len(), 1);
    assert!(remote.exists("notes/a.txt").await.expect("exists"));
    remote.delete("notes/a.txt").await.expect("delete");
    assert!(!remote.exists("notes/a.txt").await.expect("exists"));
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn local_and_remote_see_the_same_bytes() {
    let stack = start_stack("", true).await;
    let local = stack.built.get::<Handle>("local").expect("local handle");
    let remote = stack.built.get::<Handle>("remote").expect("remote handle");
    local
        .write("shared.txt", b"from local")
        .await
        .expect("write");
    assert_eq!(
        remote.read("shared.txt").await.expect("read"),
        b"from local"
    );
    remote
        .write("shared.txt", b"from remote")
        .await
        .expect("write");
    assert_eq!(
        local.read("shared.txt").await.expect("read"),
        b"from remote"
    );
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn scope_root_confines_the_remote_client() {
    let stack = start_stack("repos", true).await;
    let remote = stack.built.get::<Handle>("remote").expect("remote handle");
    remote.write("repos/ok.txt", b"ok").await.expect("in scope");
    let error = remote.read("outside.txt").await.expect_err("out of scope");
    assert_eq!(error.kind(), flowcraft::error::ErrorKind::Forbidden);
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn read_only_scope_rejects_writes() {
    let stack = start_stack("", false).await;
    let remote = stack.built.get::<Handle>("remote").expect("remote handle");
    // The local provider is still writable through the local handle.
    let local = stack.built.get::<Handle>("local").expect("local handle");
    local.write("ro.txt", b"data").await.expect("local write");
    assert_eq!(remote.read("ro.txt").await.expect("read"), b"data");
    let error = remote
        .write("ro.txt", b"nope")
        .await
        .expect_err("read-only");
    assert_eq!(error.kind(), flowcraft::error::ErrorKind::Forbidden);
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn a_revoked_session_cannot_reach_the_endpoint() {
    let stack = start_stack("", true).await;
    // Every grant of the session is invalidated, including the one the remote
    // resource minted during build.
    stack
        .platform
        .revoke_session(stack.platform.session())
        .expect("revoke");
    let remote = stack.built.get::<Handle>("remote").expect("remote handle");
    let error = remote.read("anything").await.expect_err("revoked");
    assert_eq!(error.kind(), flowcraft::error::ErrorKind::Forbidden);
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn a_client_without_a_grant_is_unauthenticated() {
    use flowcraft::kernel::GrantIssuer;

    let issuer = flowcraft::kernel::MemoryIssuer::new(std::time::Duration::from_secs(60));
    let error = GrantIssuer::validate(&issuer, "not-a-token").expect_err("no grant");
    assert_eq!(error.kind(), flowcraft::error::ErrorKind::Unauthorized);
}

#[tokio::test]
async fn capabilities_come_from_the_provider() {
    let stack = start_stack("", true).await;
    let remote = stack.built.get::<Handle>("remote").expect("remote handle");
    let capabilities = remote.capabilities().await.expect("capabilities");
    assert!(capabilities.atomic_rename);
    assert!(!capabilities.distributed);
    let _ = LocalWorkspace::open(stack._dir.path().join("probe")).expect("local");
    stack.built.shutdown().await.expect("shutdown");
}

/// The bounded read is a wire operation, not a client-side check: the endpoint
/// refuses an oversized file with the provider's own classification, so the
/// bytes never cross the socket in full.
#[tokio::test]
async fn bounded_reads_cross_the_wire() {
    let stack = start_stack("", true).await;
    let remote = stack.built.get::<Handle>("remote").expect("remote handle");
    remote.write("file", b"0123456789").await.expect("write");

    let data = remote.read_limited("file", 10).await.expect("at the cap");
    assert_eq!(data, b"0123456789");

    // A client-side fallback would answer `NotAvailable`-then-read; the served
    // RPC answers with the provider's classification instead. The wording tells
    // the two layers apart: `exceeds the … cap` comes from the provider, so it
    // proves the refusal happened before the bytes crossed the socket.
    let over = remote
        .read_limited("file", 9)
        .await
        .expect_err("over the cap");
    assert_eq!(over.kind(), flowcraft::error::ErrorKind::Validation);
    assert!(over.to_string().contains("exceeds the"), "{over}");
    let missing = remote
        .read_limited("missing", 10)
        .await
        .expect_err("missing file");
    assert_eq!(missing.kind(), flowcraft::error::ErrorKind::NotFound);

    // `capped` therefore prefers the bounded path here too, and still enforces
    // the cap for a caller that only has the composed operation.
    let data = flowcraft::caps::workspace::capped(&*remote.workspace(), "file", 10)
        .await
        .expect("capped");
    assert_eq!(data.len(), 10);
    assert_eq!(
        flowcraft::caps::workspace::capped(&*remote.workspace(), "file", 9)
            .await
            .expect_err("over the cap")
            .kind(),
        flowcraft::error::ErrorKind::Validation
    );

    stack.built.shutdown().await.expect("shutdown");
}
