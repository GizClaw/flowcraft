//! These drive unix-domain sockets and a POSIX shell; a Windows host has
//! neither in this crate yet, so the file is unix-only rather than untested.
#![cfg(unix)]
// A test may reach the filesystem directly: it has no executor to starve.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::disallowed_methods)]

//! The `craft.Craft/v1` ABI against the stub, and the exec supervisor.

mod support;

use std::path::Path;
use std::time::Duration;

use flowcraft::client::CraftClient;
use flowcraft::kernel::Target;
use flowcraft::proto::craft as pb;
use flowcraft_platformd::craft::{CraftSpec, CraftSupervisor, ExecSupervisor};

fn binary() -> &'static str {
    env!("CARGO_BIN_EXE_flowcraftd")
}

async fn wait_for_socket(path: &Path) {
    for _ in 0..250 {
        if tokio::net::UnixStream::connect(path).await.is_ok() {
            return;
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    panic!("craft stub never listened on {}", path.display());
}

#[tokio::test]
async fn the_abi_round_trips_against_the_stub() {
    let dir = tempfile::tempdir().expect("tempdir");
    let socket = dir.path().join("craft.sock");
    let mut child = support::isolated_async(tokio::process::Command::new(binary()))
        .args([
            "craft-stub",
            "--socket",
            socket.to_str().expect("path"),
            "--name",
            "demo",
        ])
        .kill_on_drop(true)
        .spawn()
        .expect("spawn stub");
    wait_for_socket(&socket).await;

    let client = CraftClient::connect(&Target::Unix(socket.clone()), None)
        .await
        .expect("connect");
    let info = client.describe().await.expect("describe");
    assert_eq!(info.name, "demo");
    assert_eq!(info.tenancy, "single");

    let started = client
        .start(pb::StartRequest {
            instance: "i-1".to_owned(),
            tenant: "t".to_owned(),
            grants: Vec::new(),
        })
        .await
        .expect("start");
    assert_eq!(started.session, "stub-session");

    let mut stream = client
        .run(pb::RunRequest {
            session: started.session.clone(),
            input: Some(flowcraft_platformd::abi::text_message(
                flowcraft::proto::message::Role::User,
                "hi",
            )),
            credit: 4,
        })
        .await
        .expect("run");
    let first = stream.message().await.expect("event").expect("event");
    assert_eq!(first.r#type, "delta");
    let last = stream.message().await.expect("event").expect("event");
    assert_eq!(last.r#type, "done");

    client
        .steer(pb::SteerRequest {
            session: started.session.clone(),
            payload: Some(flowcraft_platformd::abi::text_message(
                flowcraft::proto::message::Role::User,
                "more",
            )),
        })
        .await
        .expect("steer");
    client
        .cancel(pb::CancelRequest {
            session: started.session.clone(),
            reason: "test".to_owned(),
        })
        .await
        .expect("cancel");
    client
        .close(pb::CloseRequest {
            session: started.session.clone(),
        })
        .await
        .expect("close");

    let _ = child.kill().await;
}

#[tokio::test]
async fn the_exec_supervisor_starts_and_stops_a_craft() {
    let dir = tempfile::tempdir().expect("tempdir");
    let socket = dir.path().join("craft.sock");
    let (identity_env, identity) = support::identity_env();
    let mut spec = CraftSpec::new(
        "demo",
        vec![
            binary().to_owned(),
            "craft-stub".to_owned(),
            "--socket".to_owned(),
            socket.display().to_string(),
            "--name".to_owned(),
            "demo".to_owned(),
        ],
        Target::Unix(socket.clone()),
    );
    spec.env.insert(identity_env, identity);

    let supervisor = ExecSupervisor::new();
    let handle = supervisor.start(&spec, Vec::new()).await.expect("start");
    assert_eq!(handle.info().name, "demo");
    assert_eq!(handle.session(), "stub-session");

    let events = handle
        .run(
            flowcraft_platformd::abi::text_message(flowcraft::proto::message::Role::User, "go"),
            8,
        )
        .await
        .expect("run");
    assert_eq!(events.last().expect("event").r#type, "done");

    supervisor.stop(handle).await.expect("stop");

    // The child is gone, so nothing accepts on the socket any more.
    tokio::time::sleep(Duration::from_millis(300)).await;
    assert!(tokio::net::UnixStream::connect(&socket).await.is_err());
}

#[tokio::test]
async fn a_craft_that_never_listens_times_out() {
    let dir = tempfile::tempdir().expect("tempdir");
    let socket = dir.path().join("never.sock");
    let mut spec = CraftSpec::new(
        "sleepy",
        vec!["sleep".to_owned(), "30".to_owned()],
        Target::Unix(socket.clone()),
    );
    spec.ready_timeout = Duration::from_millis(200);
    let supervisor = ExecSupervisor::new();
    let error = supervisor
        .start(&spec, Vec::new())
        .await
        .expect_err("never listens");
    assert_eq!(error.kind(), flowcraft::error::ErrorKind::NotAvailable);
}

/// The in-process bearer: the same ABI with no socket, no token, no adapter.
///
/// This is the shape the daemon's own craft will take, so it is pinned here
/// rather than discovered later.
#[tokio::test]
async fn an_in_process_craft_needs_no_wire() {
    use flowcraft::client::CraftSession;
    use flowcraft::craft::CraftHandler;
    use flowcraft_platformd::stub::StubCraft;
    use tokio_stream::StreamExt as _;

    let handler: std::sync::Arc<dyn CraftHandler> =
        std::sync::Arc::new(StubCraft::new("builtin", None, None, None, false));
    let session = CraftSession::InProcess(handler);

    let info = session.describe().await.expect("describe");
    assert_eq!(info.name, "builtin");
    assert_eq!(info.abi_version, flowcraft_platformd::abi::ABI_VERSION);

    let started = session
        .start(pb::StartRequest {
            instance: "one".to_owned(),
            tenant: "a".to_owned(),
            grants: Vec::new(),
        })
        .await
        .expect("start");
    assert_eq!(started.session, "stub-session");

    let mut events = session
        .run(pb::RunRequest {
            session: started.session.clone(),
            input: Some(pb::Envelope {
                body: Some(pb::envelope::Body::Extension(pb::Extension {
                    r#type: "stub.text".to_owned(),
                    data: b"hi".to_vec(),
                })),
            }),
            credit: 8,
        })
        .await
        .expect("run");
    let mut kinds = Vec::new();
    while let Some(event) = events.next().await {
        kinds.push(event.expect("event").r#type);
    }
    assert_eq!(kinds, vec!["delta", "done"]);

    session
        .steer(pb::SteerRequest {
            session: started.session.clone(),
            payload: None,
        })
        .await
        .expect("steer");
    session
        .close(pb::CloseRequest {
            session: started.session,
        })
        .await
        .expect("close");
}

/// A CA plus a server certificate for `localhost`, written into `dir`.
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
    std::fs::write(dir.join("server-cert.pem"), server.pem()).expect("write cert");
    std::fs::write(dir.join("server-key.pem"), server_key.serialize_pem()).expect("write key");
}

fn free_port() -> u16 {
    let listener = std::net::TcpListener::bind("127.0.0.1:0").expect("bind");
    listener.local_addr().expect("addr").port()
}

/// The ABI over `dns` + TLS: the second bearer the stub now serves.
///
/// A craft on another host has no socket to share, so the same seven calls have
/// to work over TLS. This is the only place that path is exercised end to end.
#[tokio::test]
async fn the_abi_works_over_tls() {
    use flowcraft::kernel::config::Target as CraftTarget;
    use tonic::transport::{Certificate, ClientTlsConfig};

    // A workspace-wide build enables both rustls providers; pick the one the
    // daemon's TLS path uses.
    static ONCE: std::sync::Once = std::sync::Once::new();
    ONCE.call_once(|| {
        let _ = rustls::crypto::ring::default_provider().install_default();
    });

    let dir = tempfile::tempdir().expect("tempdir");
    pki(dir.path());
    let port = free_port();
    let address = format!("127.0.0.1:{port}");

    let mut child = support::isolated(std::process::Command::new(binary()))
        .args([
            "craft-stub",
            "--target",
            &format!("dns:///{address}"),
            "--tls-cert",
            dir.path().join("server-cert.pem").to_str().expect("path"),
            "--tls-key",
            dir.path().join("server-key.pem").to_str().expect("path"),
            "--name",
            "remote",
        ])
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .spawn()
        .expect("spawn stub");

    let mut listening = false;
    for _ in 0..250 {
        if std::net::TcpStream::connect(&address).is_ok() {
            listening = true;
            break;
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    if !listening {
        let _ = child.kill();
        panic!("craft stub never listened on {address}");
    }

    let ca = std::fs::read(dir.path().join("ca.pem")).expect("read ca");
    let tls = ClientTlsConfig::new()
        .ca_certificate(Certificate::from_pem(ca))
        .domain_name("localhost");
    let client = match CraftClient::connect(&CraftTarget::Dns(address), Some(tls)).await {
        Ok(client) => client,
        Err(error) => {
            let _ = child.kill();
            panic!("connect over tls: {error}");
        }
    };

    let info = client.describe().await.expect("describe");
    assert_eq!(info.name, "remote");
    let started = client
        .start(pb::StartRequest {
            instance: "one".to_owned(),
            tenant: "a".to_owned(),
            grants: Vec::new(),
        })
        .await
        .expect("start");
    assert_eq!(started.session, "stub-session");

    let mut events = client
        .run(pb::RunRequest {
            session: started.session.clone(),
            input: None,
            credit: 4,
        })
        .await
        .expect("run");
    let first = events.message().await.expect("event").expect("some");
    assert_eq!(first.r#type, "delta");

    client
        .close(pb::CloseRequest {
            session: started.session,
        })
        .await
        .expect("close");
    let _ = child.kill();
    let _ = child.wait();
}

/// A craft child does not inherit the daemon's environment.
///
/// The child runs with the daemon's uid, so anything in that environment is
/// something the craft can read — the bootstrap secrets when the host configured
/// them as `FLOWCRAFT_SECRET_*`, this process's identity path, another craft's
/// token. The parent here is the test process, which `cargo` fills with
/// `CARGO_*`: none of them may reach the child, while the allow-list and
/// `crafts.<name>.env` still must.
#[cfg(unix)]
#[tokio::test]
async fn a_craft_child_does_not_inherit_the_daemons_environment() {
    use std::os::unix::fs::PermissionsExt as _;

    let dir = tempfile::tempdir().expect("tempdir");
    let socket = dir.path().join("craft.sock");
    let dump = dir.path().join("env.txt");
    // A shell that records its environment, then becomes the stub.
    let script = dir.path().join("craft.sh");
    std::fs::write(
        &script,
        format!(
            "#!/bin/sh\nenv > '{dump}'\nexec '{bin}' craft-stub --socket '{socket}'\n",
            dump = dump.display(),
            bin = binary(),
            socket = socket.display(),
        ),
    )
    .expect("write script");
    std::fs::set_permissions(&script, std::fs::Permissions::from_mode(0o755)).expect("chmod");

    let mut spec = CraftSpec::new(
        "oracle",
        vec![script.display().to_string()],
        Target::Unix(socket.clone()),
    );
    spec.env = std::collections::BTreeMap::from([support::identity_env()]);

    let supervisor = ExecSupervisor::new();
    let handle = supervisor
        .start(&spec, Vec::new())
        .await
        .expect("start the craft");
    let seen = std::fs::read_to_string(&dump).expect("read the child's environment");
    supervisor.stop(handle).await.expect("stop");

    for leaked in ["CARGO_MANIFEST_DIR", "CARGO_PKG_NAME", "RUST_BACKTRACE"] {
        assert!(
            !seen.contains(&format!("{leaked}=")),
            "the craft inherited {leaked}:\n{seen}"
        );
    }
    assert!(seen.contains("PATH="), "a child needs its PATH:\n{seen}");
    assert!(
        seen.contains("FLOWCRAFT_IDENTITY_PATH="),
        "crafts.<name>.env is the sanctioned way to pass one:\n{seen}"
    );
}
