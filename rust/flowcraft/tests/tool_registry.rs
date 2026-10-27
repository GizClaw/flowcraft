// A test may reach the filesystem directly: it has no executor to starve.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::disallowed_methods)]

//! End-to-end: a `tool.Registry` resource merges a capability source and feeds
//! a `tool.Endpoint`, which a remote client calls over a real unix socket.

use std::collections::BTreeMap;
use std::sync::Arc;

use flowcraft::caps::tool::resource::RegistryFactory;
use flowcraft::caps::tool::{CONTRACT, Handle, LocalSource, endpoint, remote};
use flowcraft::kernel::handles::contracts;
use flowcraft::kernel::{Config, EndpointConfig, EndpointHost, Mode, Platform, RemoteConfig};
use flowcraft::message::text_content;
use flowcraft::proto::message as msg;
use flowcraft::resource::Deployment;
use flowcraft::resource::{Builder, ExternalResource, Registry};
use serde_json::json;

fn definition(name: &str, description: &str) -> msg::ToolDefinition {
    msg::ToolDefinition {
        name: name.to_owned(),
        description: description.to_owned(),
        input_schema: Vec::new(),
    }
}

fn local() -> Arc<LocalSource> {
    Arc::new(
        LocalSource::new()
            .with(
                definition("echo", "repeat the arguments"),
                Arc::new(|args: &str| Ok(text_content(args))),
            )
            .expect("echo")
            .with(
                definition("clock", "the current time"),
                Arc::new(|_: &str| Ok(text_content("12:00"))),
            )
            .expect("clock"),
    )
}

#[tokio::test]
async fn a_registry_resource_feeds_an_endpoint() {
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
    registry
        .register(RegistryFactory)
        .expect("registry factory");

    let document: Deployment = flowcraft::resource::decode::deployment(
        serde_json::to_vec(&json!({
            "version": "v1",
            "resources": {
                "tools": {
                    "kind": "tool.Registry",
                    "impl": "memory",
                    "deps": {"tool.local": "source"}
                },
                "endpoint": {
                    "kind": "tool.Endpoint",
                    "impl": "grpc",
                    "deps": {"source": "tools", "host": "host", "grants": "grants"},
                    "settings": {"endpoint": "tools"}
                },
                "client": {
                    "kind": "tool.Source",
                    "impl": "remote",
                    "deps": {"endpoints": "endpoints"},
                    "settings": {"endpoint": "tools", "scope": {"allow": ["echo"]}}
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

    let client = built.get::<Handle>("client").expect("client");
    // The registry merged the capability source; the allow-list hides `clock`
    // from the listing but the tool is still reachable by exact name.
    let listed: Vec<String> = client
        .list()
        .await
        .expect("list")
        .into_iter()
        .map(|definition| definition.name)
        .collect();
    assert_eq!(listed, vec!["echo"]);

    let content = client
        .execute("echo", r#"{"query":"rust"}"#)
        .await
        .expect("execute");
    assert_eq!(content, text_content(r#"{"query":"rust"}"#));

    built.shutdown().await.expect("shutdown");
}
