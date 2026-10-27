#![allow(clippy::unwrap_used, clippy::expect_used)]

//! The admin socket: status, reload, drain, revoke, and the `ctl` client.

mod support;

use std::collections::BTreeMap;
use std::path::Path;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};

use flowcraft::kernel::{Config, EndpointConfig, Mode, Platform, RemoteConfig};
use flowcraft_platformd::admin::{AdminState, serve};
use flowcraft_platformd::supervisor::Supervisor;
use serde_json::{Value, json};

fn platform(dir: &Path) -> Platform {
    let config = Config {
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
    };
    Platform::new(config).expect("platform")
}

fn document(dir: &Path) -> Vec<u8> {
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
                "deps": {"workspace": "local"},
                "settings": {"endpoint": "ws"}
            }
        },
        "entries": ["local"]
    }))
    .expect("json")
}

/// One command through the production client — the same code `ctl` runs.
async fn send(socket: &Path, request: Value) -> Value {
    let command = request
        .get("command")
        .and_then(Value::as_str)
        .expect("command");
    let session = request.get("session").and_then(Value::as_str);
    let client = flowcraft_platformd::commands::AdminClient {
        target: socket.display().to_string(),
        ca: None,
        server_name: None,
        token_file: None,
        token: None,
    };
    flowcraft_platformd::commands::call(&client, command, session)
        .await
        .expect("admin call")
}

struct Fixture {
    _dir: tempfile::TempDir,
    deploy: std::path::PathBuf,
    socket: std::path::PathBuf,
    _handle: flowcraft_platformd::admin::AdminHandle,
    ready: Arc<AtomicBool>,
}

async fn fixture() -> Fixture {
    let dir = tempfile::tempdir().expect("tempdir");
    let deploy = dir.path().join("deploy.json");
    std::fs::write(&deploy, document(dir.path())).expect("deploy");
    let supervisor = Supervisor::boot(platform(dir.path()), vec![deploy.clone()])
        .await
        .expect("boot");
    let socket = dir.path().join("admin.sock");
    let ready = Arc::new(AtomicBool::new(true));
    let state = AdminState::new(
        supervisor,
        vec!["demo".to_owned()],
        ready.clone(),
        1,
        Arc::new(flowcraft_platformd::logging::Applied::disabled()),
    );
    // A unix socket: the peer credentials are the gate, so no TLS and no token.
    let handle = serve(
        &flowcraft::kernel::Target::Unix(socket.clone()),
        None,
        None,
        state,
    )
    .await
    .expect("admin");
    Fixture {
        _dir: dir,
        deploy,
        socket,
        _handle: handle,
        ready,
    }
}

#[tokio::test]
async fn diag_reports_the_pipeline_without_its_secrets() {
    let fixture = fixture().await;
    let response = send(&fixture.socket, json!({"command": "diag"})).await;
    assert_eq!(response["ok"], true, "{response}");
    assert_eq!(response["service"], "flowcraftd");
    assert!(
        response["session"]
            .as_str()
            .is_some_and(|id| id.starts_with("s-"))
    );
    assert!(response.get("ring").is_some());
    // The identity salt and any grant token must never travel over this socket.
    let text = response.to_string();
    assert!(!text.contains("salt"), "{text}");
    assert!(!text.contains("token"), "{text}");
}

#[tokio::test]
async fn status_reload_drain_and_revoke() {
    let fixture = fixture().await;

    let status = send(&fixture.socket, json!({"command": "status"})).await;
    assert_eq!(status["ok"], true, "{status}");
    assert_eq!(status["generation"], 1);
    assert_eq!(status["ready"], true);
    assert_eq!(status["crafts"][0], "demo");
    assert_eq!(status["endpoints"], 1);
    assert_eq!(status["entries"][0], "local");
    // The bounds next to the readings that approach them. This fixture has no
    // sandbox runner, so the list is empty and the grant table is present — the
    // shape an operator's dashboard reads either way.
    assert_eq!(status["sandbox"]["grants_revoked"], 0);
    assert!(
        status["sandbox"]["runners"].is_array(),
        "the runner list must exist even when empty: {status}"
    );

    let reloaded = send(&fixture.socket, json!({"command": "reload"})).await;
    assert_eq!(reloaded["ok"], true, "{reloaded}");
    assert_eq!(reloaded["previous"], 1);
    assert_eq!(reloaded["current"], 2);
    assert_eq!(reloaded["drained"], true, "{reloaded}");

    let status = send(&fixture.socket, json!({"command": "status"})).await;
    assert_eq!(status["generation"], 2);

    let drained = send(&fixture.socket, json!({"command": "drain"})).await;
    assert_eq!(drained["ready"], false);
    assert!(!fixture.ready.load(Ordering::SeqCst));

    // A verb with no route never reaches the daemon: the client refuses to build
    // a request for it, and an unknown *path* is a 404 on the server (pinned in
    // `admin::tests::the_routes_are_the_six_verbs`).
    let client = flowcraft_platformd::commands::AdminClient {
        target: fixture.socket.display().to_string(),
        ca: None,
        server_name: None,
        token_file: None,
        token: None,
    };
    let refused = flowcraft_platformd::commands::call(&client, "nope", None).await;
    let message = refused.expect_err("no route for nope").to_string();
    assert!(message.contains("nope"), "{message}");

    let revoke = send(
        &fixture.socket,
        json!({"command": "revoke", "session": "bootstrap"}),
    )
    .await;
    assert_eq!(revoke["ok"], true, "{revoke}");

    // A reload after a plan error reports the error and keeps the generation.
    std::fs::write(&fixture.deploy, b"{ broken").expect("write");
    let failed = send(&fixture.socket, json!({"command": "reload"})).await;
    assert_eq!(failed["ok"], false);
    let status = send(&fixture.socket, json!({"command": "status"})).await;
    assert_eq!(status["generation"], 2);
}

#[tokio::test]
async fn the_ctl_binary_talks_to_the_socket() {
    let fixture = fixture().await;
    let output = support::isolated_async(tokio::process::Command::new(support::BIN))
        .args([
            "ctl",
            "--socket",
            fixture.socket.to_str().expect("path"),
            "status",
        ])
        .output()
        .await
        .expect("ctl");
    assert!(
        output.status.success(),
        "stderr: {}",
        String::from_utf8_lossy(&output.stderr)
    );
    let text = String::from_utf8_lossy(&output.stdout);
    assert!(text.contains("\"generation\": 1"), "{text}");

    let output = support::isolated_async(tokio::process::Command::new(support::BIN))
        .args([
            "ctl",
            "--socket",
            fixture.socket.to_str().expect("path"),
            "reload",
        ])
        .output()
        .await
        .expect("ctl reload");
    assert!(output.status.success());
    let text = String::from_utf8_lossy(&output.stdout);
    assert!(text.contains("\"current\": 2"), "{text}");

    let output = support::isolated_async(tokio::process::Command::new(support::BIN))
        .args([
            "ctl",
            "--socket",
            fixture.socket.to_str().expect("path"),
            "nope",
        ])
        .output()
        .await
        .expect("ctl nope");
    assert!(!output.status.success());
}

/// A CA plus a server certificate for `localhost`.
fn pki(dir: &Path) {
    use rcgen::{
        BasicConstraints, CertificateParams, ExtendedKeyUsagePurpose, IsCa, Issuer, KeyPair,
        KeyUsagePurpose,
    };

    let ca_key = KeyPair::generate().expect("ca key");
    let mut ca_params = CertificateParams::new(Vec::new()).expect("ca params");
    ca_params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
    ca_params.key_usages = vec![KeyUsagePurpose::KeyCertSign, KeyUsagePurpose::CrlSign];
    let ca = ca_params.self_signed(&ca_key).expect("ca cert");
    let issuer = Issuer::new(ca_params, ca_key);
    std::fs::write(dir.join("ca.pem"), ca.pem()).expect("write ca");

    let server_key = KeyPair::generate().expect("server key");
    let mut server_params =
        CertificateParams::new(vec!["localhost".to_owned()]).expect("server params");
    server_params
        .extended_key_usages
        .push(ExtendedKeyUsagePurpose::ServerAuth);
    let server = server_params
        .signed_by(&server_key, &issuer)
        .expect("server cert");
    std::fs::write(dir.join("cert.pem"), server.pem()).expect("write cert");
    std::fs::write(dir.join("key.pem"), server_key.serialize_pem()).expect("write key");
}

fn free_port() -> u16 {
    let listener = std::net::TcpListener::bind("127.0.0.1:0").expect("bind");
    listener.local_addr().expect("addr").port()
}

/// The `dns` admin target: TLS plus a bearer token, and the token is what makes
/// the surface owner-only on a host without unix sockets.
#[tokio::test]
async fn a_dns_admin_target_serves_http_over_tls_with_a_token() {
    use flowcraft::kernel::config::TlsConfig;

    let dir = tempfile::tempdir().expect("tempdir");
    let pki_dir = dir.path().join("secrets");
    std::fs::create_dir_all(&pki_dir).expect("pki dir");
    pki(&pki_dir);
    let token_path = dir.path().join("admin.token");
    let port = free_port();

    // The host configuration names references, never bytes; the resolver is the
    // bootstrap provider, the same source endpoint TLS material comes from.
    let config = Config {
        listen: flowcraft::kernel::config::ListenConfig {
            admin: format!("dns:///127.0.0.1:{port}"),
            admin_tls: Some(TlsConfig {
                cert: "${secret:cert.pem}".to_owned(),
                key: "${secret:key.pem}".to_owned(),
                ..TlsConfig::default()
            }),
            admin_token_file: token_path.display().to_string(),
        },
        bootstrap: flowcraft::kernel::config::BootstrapConfig {
            dir: pki_dir.display().to_string(),
            ..flowcraft::kernel::config::BootstrapConfig::default()
        },
        ..Config::default()
    };
    config
        .validate()
        .expect("a dns admin target with tls and a token");

    // A plaintext admin target is refused at startup, not at connect time.
    let mut plaintext = config.clone();
    plaintext.listen.admin_tls = None;
    assert!(
        plaintext
            .validate()
            .expect_err("tls is mandatory")
            .to_string()
            .contains("admin_tls"),
        "the refusal names the missing block"
    );
    let mut tokenless = config.clone();
    tokenless.listen.admin_token_file = String::new();
    assert!(
        tokenless
            .validate()
            .expect_err("a token is mandatory")
            .to_string()
            .contains("admin_token_file")
    );

    // Serve it, with the token file the daemon would have written.
    let token = flowcraft_platformd::admin::mint_token();
    flowcraft_platformd::admin::write_token(&token_path, &token).expect("write token");
    let acceptor = flowcraft_platformd::admin::acceptor(
        &std::fs::read(pki_dir.join("cert.pem")).expect("cert"),
        &std::fs::read(pki_dir.join("key.pem")).expect("key"),
    )
    .expect("acceptor");

    let deploy = dir.path().join("deploy.json");
    std::fs::write(&deploy, document(dir.path())).expect("deploy");
    let supervisor = Supervisor::boot(platform(dir.path()), vec![deploy])
        .await
        .expect("boot");
    let state = AdminState::new(
        supervisor,
        vec!["demo".to_owned()],
        Arc::new(AtomicBool::new(true)),
        1,
        Arc::new(flowcraft_platformd::logging::Applied::disabled()),
    );
    let handle = serve(
        &flowcraft::kernel::Target::Dns(format!("127.0.0.1:{port}")),
        Some(acceptor),
        Some(token),
        state,
    )
    .await
    .expect("admin listener");

    let client = flowcraft_platformd::commands::AdminClient {
        target: format!("dns:///127.0.0.1:{port}"),
        ca: Some(pki_dir.join("ca.pem").display().to_string()),
        server_name: Some("localhost".to_owned()),
        token_file: Some(token_path.display().to_string()),
        token: None,
    };
    let status = flowcraft_platformd::commands::call(&client, "status", None)
        .await
        .expect("status over tls");
    assert_eq!(status["ok"], true);
    assert_eq!(status["crafts"], json!(["demo"]));

    // The same request without the token is refused by the listener, not by a
    // route: the transport is what carries "the same user" here.
    let anonymous = flowcraft_platformd::commands::AdminClient {
        target: client.target.clone(),
        ca: client.ca.clone(),
        server_name: client.server_name.clone(),
        token_file: None,
        token: Some("not-the-token".to_owned()),
    };
    let refused = flowcraft_platformd::commands::call(&anonymous, "status", None)
        .await
        .expect("a refusal is still an answer");
    assert_eq!(refused["ok"], false);
    assert!(
        refused["error"]
            .as_str()
            .unwrap_or_default()
            .contains("token"),
        "{refused}"
    );

    handle.stop().await;
}
