#![allow(clippy::unwrap_used, clippy::expect_used)]

//! End-to-end: local registry → endpoint server → remote client, over a real
//! unix socket, with grants, an allow-list, and a call budget.

use std::collections::BTreeMap;
use std::sync::Arc;

use flowcraft::caps::tool::{CONTRACT, Handle, LocalSource, endpoint, remote, text_result};
use flowcraft::kernel::handles::contracts;
use flowcraft::kernel::{Config, EndpointConfig, EndpointHost, Mode, Platform, RemoteConfig};
use flowcraft::proto::message as msg;
use flowcraft::resource::Deployment;
use flowcraft::resource::{Builder, ExternalResource, Registry};
use serde_json::json;

struct Stack {
    _dir: tempfile::TempDir,
    platform: Platform,
    built: flowcraft::resource::Built,
}

fn definition(name: &str, description: &str) -> msg::ToolDefinition {
    msg::ToolDefinition {
        name: name.to_owned(),
        description: description.to_owned(),
        input_schema: Vec::new(),
    }
}

fn local() -> Arc<LocalSource> {
    let source = LocalSource::new()
        .with(
            definition("echo", "repeat the arguments"),
            Arc::new(|args: &str| Ok(text_result(args))),
        )
        .expect("echo")
        .with(
            definition("clock", "the current time"),
            Arc::new(|_: &str| Ok(text_result("12:00"))),
        )
        .expect("clock");
    Arc::new(source)
}

async fn start(scope: serde_json::Value) -> Stack {
    let dir = tempfile::tempdir().expect("tempdir");
    let socket = dir.path().join("tools.sock");
    let config = Config {
        remote: RemoteConfig {
            mode: Mode::Trusted,
            allow_targets: Vec::new(),
            endpoints: BTreeMap::from([(
                "tools".to_owned(),
                EndpointConfig {
                    target: format!("unix://{}", socket.display()),
                    tenant: String::new(),
                    capabilities: vec!["tool.ToolSource/v1".to_owned()],
                    tls: None,
                },
            )]),
        },
        ..Config::default()
    };
    let platform = Platform::new(config).expect("platform");

    let mut registry = Registry::new();
    registry
        .register(endpoint::EndpointFactory)
        .expect("endpoint");
    registry.register(remote::RemoteFactory).expect("remote");

    let document: Deployment = flowcraft::resource::decode::deployment(
        serde_json::to_vec(&json!({
            "version": "v1",
            "resources": {
                "tools": {
                    "kind": "tool.Endpoint",
                    "impl": "grpc",
                    "deps": {"source": "source", "host": "host", "grants": "grants"},
                    "settings": {"endpoint": "tools"}
                },
                "client": {
                    "kind": "tool.Source",
                    "impl": "remote",
                    "deps": {"endpoints": "endpoints"},
                    "settings": {"endpoint": "tools", "scope": scope}
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
            ExternalResource::new("source", CONTRACT, Arc::new(Handle::new(local()))),
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
async fn list_and_execute_round_trip() {
    let stack = start(json!({})).await;
    let client = stack.built.get::<Handle>("client").expect("client");
    let names: Vec<String> = client
        .list()
        .await
        .expect("list")
        .into_iter()
        .map(|item| item.name)
        .collect();
    assert_eq!(names, vec!["clock", "echo"]);

    let content = client
        .execute("echo", r#"{"query":"rust"}"#)
        .await
        .expect("execute");
    assert_eq!(content, text_result(r#"{"query":"rust"}"#));
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn the_allow_list_confines_listing_and_calls() {
    let stack = start(json!({"allow": ["echo"]})).await;
    let client = stack.built.get::<Handle>("client").expect("client");
    let names: Vec<String> = client
        .list()
        .await
        .expect("list")
        .into_iter()
        .map(|item| item.name)
        .collect();
    assert_eq!(names, vec!["echo"]);

    let error = client
        .execute("clock", "{}")
        .await
        .expect_err("out of scope");
    assert_eq!(error.kind(), flowcraft::error::ErrorKind::Forbidden);
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn the_call_budget_is_charged_per_grant() {
    let stack = start(json!({"max_calls": 2})).await;
    let client = stack.built.get::<Handle>("client").expect("client");
    client.execute("echo", "{}").await.expect("first");
    client.execute("echo", "{}").await.expect("second");
    let error = client.execute("echo", "{}").await.expect_err("exhausted");
    assert_eq!(error.kind(), flowcraft::error::ErrorKind::RateLimit);
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn an_unknown_tool_is_not_found() {
    let stack = start(json!({})).await;
    let client = stack.built.get::<Handle>("client").expect("client");
    let error = client.execute("missing", "{}").await.expect_err("missing");
    assert_eq!(error.kind(), flowcraft::error::ErrorKind::NotFound);
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn invalid_arguments_are_refused_by_the_provider() {
    let stack = start(json!({})).await;
    let client = stack.built.get::<Handle>("client").expect("client");
    let error = client.execute("echo", "{oops").await.expect_err("bad args");
    assert_eq!(error.kind(), flowcraft::error::ErrorKind::Validation);
    stack.built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn a_revoked_session_cannot_call() {
    let stack = start(json!({})).await;
    stack
        .platform
        .revoke_session(stack.platform.session())
        .expect("revoke");
    let client = stack.built.get::<Handle>("client").expect("client");
    let error = client.execute("echo", "{}").await.expect_err("revoked");
    assert_eq!(error.kind(), flowcraft::error::ErrorKind::Forbidden);
    stack.built.shutdown().await.expect("shutdown");
}
