// A test may reach the filesystem directly: it has no executor to starve.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::disallowed_methods)]

//! A real TLS/mTLS round trip: generated CA, server certificate for
//! `sandbox.test`, client identity, endpoint on `dns://`, and a remote client
//! that verifies the server and presents its own certificate.

use std::path::Path;
use std::sync::Arc;

use flowcraft::caps::workspace::{Handle, endpoint, local, remote};
use flowcraft::kernel::secret_provider::FileProvider;
use flowcraft::kernel::{Config as HostConfig, Platform};
use flowcraft::resource::Deployment;
use flowcraft::resource::{Builder, ExternalResource, Registry};
use rcgen::{
    BasicConstraints, CertificateParams, ExtendedKeyUsagePurpose, IsCa, Issuer, KeyPair,
    KeyUsagePurpose,
};

fn write(path: &Path, contents: impl AsRef<[u8]>) {
    std::fs::write(path, contents).expect("write pem");
}

/// CA + a server cert for `sandbox.test` + a client cert, all in `dir`.
fn pki(dir: &Path) {
    let ca_key = KeyPair::generate().expect("ca key");
    let mut ca_params = CertificateParams::new(Vec::new()).expect("ca params");
    ca_params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
    ca_params.key_usages = vec![KeyUsagePurpose::KeyCertSign, KeyUsagePurpose::CrlSign];
    let ca = ca_params.self_signed(&ca_key).expect("ca cert");
    let issuer = Issuer::new(ca_params, ca_key);
    write(&dir.join("ca.pem"), ca.pem());

    let server_key = KeyPair::generate().expect("server key");
    let mut server_params =
        CertificateParams::new(vec!["sandbox.test".to_owned()]).expect("server params");
    server_params
        .extended_key_usages
        .push(ExtendedKeyUsagePurpose::ServerAuth);
    let server = server_params
        .signed_by(&server_key, &issuer)
        .expect("server cert");
    write(&dir.join("server-cert.pem"), server.pem());
    write(&dir.join("server-key.pem"), server_key.serialize_pem());

    let client_key = KeyPair::generate().expect("client key");
    let mut client_params =
        CertificateParams::new(vec!["client.sandbox.test".to_owned()]).expect("client params");
    client_params
        .extended_key_usages
        .push(ExtendedKeyUsagePurpose::ClientAuth);
    let client = client_params
        .signed_by(&client_key, &issuer)
        .expect("client cert");
    write(&dir.join("client-cert.pem"), client.pem());
    write(&dir.join("client-key.pem"), client_key.serialize_pem());
}

fn free_port() -> u16 {
    let listener = std::net::TcpListener::bind("127.0.0.1:0").expect("bind");
    listener.local_addr().expect("addr").port()
}

/// Pin the process-wide rustls provider.
///
/// A workspace-wide build unifies rustls' `ring` feature (what tonic's
/// `tls-ring` enables) with `aws_lc_rs` (what the telemetry OTLP client
/// enables), and rustls then refuses to pick one implicitly. The daemon's TLS
/// path is the ring one.
fn pin_crypto_provider() {
    static ONCE: std::sync::Once = std::sync::Once::new();
    ONCE.call_once(|| {
        let _ = rustls::crypto::ring::default_provider().install_default();
    });
}

fn platform(dir: &Path, with_identity: bool) -> Platform {
    pin_crypto_provider();
    let port = free_port();
    let identity = if with_identity {
        "        client_cert: \"${secret:client-cert.pem}\"\n        client_key: \"${secret:client-key.pem}\"\n"
    } else {
        ""
    };
    let document = format!(
        r#"
remote:
  mode: trusted
  endpoints:
    ws:
      target: "dns:///127.0.0.1:{port}"
      capabilities: ["workspace.Workspace/v1"]
      tls:
        cert: "${{secret:server-cert.pem}}"
        key: "${{secret:server-key.pem}}"
        client_ca: "${{secret:ca.pem}}"
        ca: "${{secret:ca.pem}}"
{identity}        server_name: sandbox.test
"#
    );
    let config = HostConfig::parse(document.as_bytes()).expect("config");
    Platform::with_secrets(
        config,
        Arc::new(FileProvider::new(dir.to_path_buf()).expect("provider")),
    )
    .expect("platform")
}

fn build(_platform: &Platform) -> Registry {
    let mut registry = Registry::new();
    registry.register(local::LocalFactory).expect("local");
    registry
        .register(endpoint::EndpointFactory)
        .expect("endpoint");
    registry.register(remote::RemoteFactory).expect("remote");
    registry
}

#[tokio::test]
async fn mtls_round_trip_over_tcp() {
    let dir = tempfile::tempdir().expect("tempdir");
    pki(dir.path());
    let platform = platform(dir.path(), true);
    let registry = build(&platform);

    let document: Deployment = flowcraft::resource::decode::deployment(
        serde_json::to_vec(&serde_json::json!({
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
                "client": {
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

    let built = Builder::new(Arc::new(registry))
        .with_externals([
            ExternalResource::new(
                "host",
                flowcraft::kernel::handles::contracts::ENDPOINT_HOST,
                platform.endpoint_host(),
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

    let client = built.get::<Handle>("client").expect("client");
    client.write("tls.txt", b"over tls").await.expect("write");
    assert_eq!(client.read("tls.txt").await.expect("read"), b"over tls");
    built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn a_client_without_an_identity_is_refused() {
    let dir = tempfile::tempdir().expect("tempdir");
    pki(dir.path());
    let platform = platform(dir.path(), false);
    let registry = build(&platform);

    let document: Deployment = flowcraft::resource::decode::deployment(
        serde_json::to_vec(&serde_json::json!({
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
                "client": {
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

    let built = Builder::new(Arc::new(registry))
        .with_externals([
            ExternalResource::new(
                "host",
                flowcraft::kernel::handles::contracts::ENDPOINT_HOST,
                platform.endpoint_host(),
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

    let client = built.get::<Handle>("client").expect("client");
    let error = client.read("anything").await.expect_err("no client cert");
    assert_eq!(error.kind(), flowcraft::error::ErrorKind::NotAvailable);
    let _ = built.shutdown().await;
}
