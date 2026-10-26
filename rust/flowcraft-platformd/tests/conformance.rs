#![allow(clippy::unwrap_used, clippy::expect_used)]

//! The craft ABI conformance suite, plus the platform's contract enforcement.
//!
//! `assert_craft_abi` is the reusable part: any craft implementation can be
//! pointed at it. The stub is the reference implementation.

mod support;

use std::collections::BTreeMap;
use std::path::Path;
use std::sync::Arc;
use std::time::Duration;

use flowcraft::client::CraftClient;
use flowcraft::kernel::Target;
use flowcraft::proto::craft as pb;
use flowcraft_platformd::abi;
use flowcraft_platformd::hosting::HostedCrafts;

const TOKEN: &str = "conformance-token";

fn binary() -> &'static str {
    env!("CARGO_BIN_EXE_flowcraftd")
}

fn stub_argv(socket: &Path, extra: &[&str]) -> Vec<String> {
    let mut argv = vec![
        binary().to_owned(),
        "craft-stub".to_owned(),
        "--socket".to_owned(),
        socket.display().to_string(),
        "--name".to_owned(),
        "conformance".to_owned(),
        "--token".to_owned(),
        TOKEN.to_owned(),
    ];
    argv.extend(extra.iter().map(|arg| (*arg).to_owned()));
    argv
}

async fn spawn_stub(socket: &Path, extra: &[&str]) -> tokio::process::Child {
    let argv = stub_argv(socket, extra);
    let child = support::isolated_async(tokio::process::Command::new(&argv[0]))
        .args(&argv[1..])
        .kill_on_drop(true)
        .spawn()
        .expect("spawn stub");
    for _ in 0..250 {
        if tokio::net::UnixStream::connect(socket).await.is_ok() {
            return child;
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    panic!("stub never listened on {}", socket.display());
}

async fn connect(socket: &Path, token: Option<&str>) -> CraftClient {
    CraftClient::connect_with_token(&Target::Unix(socket.to_path_buf()), None, token)
        .await
        .expect("connect")
}

/// The conformance checks every craft must pass.
async fn assert_craft_abi(client: &CraftClient) {
    // 1. Describe declares a compatible ABI and its envelope types.
    let info = client.describe().await.expect("describe");
    assert_eq!(info.abi_version, abi::ABI_VERSION);
    assert!(!info.input_types.is_empty(), "input_types must be declared");
    assert!(!info.event_types.is_empty(), "event_types must be declared");
    abi::validate_info(&info).expect("valid info");

    // 2. Start opens a session.
    let started = client
        .start(pb::StartRequest {
            instance: "conformance".to_owned(),
            tenant: "t".to_owned(),
            grants: Vec::new(),
        })
        .await
        .expect("start");
    assert!(!started.session.is_empty());

    // 3. Run streams valid envelopes and terminates.
    let input = abi::text_message(flowcraft::proto::message::Role::User, "hello");
    let mut stream = client
        .run(pb::RunRequest {
            session: started.session.clone(),
            input: Some(input),
            credit: 4,
        })
        .await
        .expect("run");
    let mut terminal = false;
    while let Some(event) = stream.message().await.expect("event") {
        abi::validate_event(&info, &event).expect("valid event");
        if event.r#type == "done" || event.r#type == "error" {
            terminal = true;
            break;
        }
    }
    assert!(terminal, "a Run stream must end with a terminal event");

    // 4. Renew is accepted for the session.
    client
        .renew(pb::RenewRequest {
            session: started.session.clone(),
            grants: Vec::new(),
        })
        .await
        .expect("renew");

    // 5. Close releases the session; a later Run is refused.
    client
        .close(pb::CloseRequest {
            session: started.session.clone(),
        })
        .await
        .expect("close");
    let refused = client
        .run(pb::RunRequest {
            session: started.session,
            input: Some(abi::text_message(
                flowcraft::proto::message::Role::User,
                "again",
            )),
            credit: 1,
        })
        .await;
    assert!(refused.is_err(), "a closed session must refuse Run");
}

#[tokio::test]
async fn the_stub_passes_the_conformance_suite() {
    let dir = tempfile::tempdir().expect("tempdir");
    let socket = dir.path().join("craft.sock");
    let mut child = spawn_stub(&socket, &[]).await;
    let client = connect(&socket, Some(TOKEN)).await;
    assert_craft_abi(&client).await;
    let _ = child.kill().await;
}

#[tokio::test]
async fn after_describe_every_call_needs_the_bootstrap_token() {
    let dir = tempfile::tempdir().expect("tempdir");
    let socket = dir.path().join("craft.sock");
    let mut child = spawn_stub(&socket, &[]).await;
    let anonymous = connect(&socket, None).await;
    // Describe is open: it is how a platform learns what to expect.
    anonymous.describe().await.expect("describe");
    let refused = anonymous
        .start(pb::StartRequest {
            instance: "i".to_owned(),
            tenant: "t".to_owned(),
            grants: Vec::new(),
        })
        .await;
    assert!(refused.is_err(), "Start without the token must fail");
    let _ = child.kill().await;
}

/// The platform generates the bootstrap token for `exec` crafts and hands it to
/// the child through the environment, so the stub must not also pin one.
fn hosted_stub_argv(socket: &Path, extra: &[&str]) -> Vec<String> {
    let mut argv = vec![
        binary().to_owned(),
        "craft-stub".to_owned(),
        "--socket".to_owned(),
        socket.display().to_string(),
        "--name".to_owned(),
        "conformance".to_owned(),
    ];
    argv.extend(extra.iter().map(|arg| (*arg).to_owned()));
    argv
}

async fn hosted(socket: &Path, extra: &[&str]) -> Result<HostedCrafts, flowcraft::error::Error> {
    let config = flowcraft::kernel::Config {
        crafts: BTreeMap::from([(
            "demo".to_owned(),
            flowcraft::kernel::CraftConfig {
                argv: hosted_stub_argv(socket, extra),
                socket: socket.display().to_string(),
                tenant: "t".to_owned(),
                grants: Vec::new(),
                env: BTreeMap::from([support::identity_env()]),
                ready_timeout_secs: 10,
                supervisor: "exec".to_owned(),
                unit: String::new(),
                systemctl: String::new(),
            },
        )]),
        ..flowcraft::kernel::Config::default()
    };
    config.validate().expect("config");
    let platform = Arc::new(flowcraft::kernel::Platform::new(config).expect("platform"));
    HostedCrafts::start(platform.clone(), &platform.config().crafts).await
}

#[tokio::test]
async fn an_incompatible_abi_is_refused_at_start() {
    let dir = tempfile::tempdir().expect("tempdir");
    let socket = dir.path().join("craft.sock");
    let error = hosted(&socket, &["--abi-version", "flowcraft.craft/v9"])
        .await
        .expect_err("incompatible ABI");
    assert_eq!(error.kind(), flowcraft::error::ErrorKind::NotAvailable);
}

#[tokio::test]
async fn a_platform_capability_cannot_be_advertised() {
    let dir = tempfile::tempdir().expect("tempdir");
    let socket = dir.path().join("craft.sock");
    let error = hosted(&socket, &["--provides", "workspace.Workspace/v1"])
        .await
        .expect_err("platform namespace");
    assert_eq!(error.kind(), flowcraft::error::ErrorKind::Forbidden);
}

#[tokio::test]
async fn a_private_capability_is_accepted() {
    let dir = tempfile::tempdir().expect("tempdir");
    let socket = dir.path().join("craft.sock");
    let hosted = hosted(&socket, &["--provides", "acme.conformance.tools/v1"])
        .await
        .expect("private namespace");
    assert_eq!(hosted.names(), vec!["conformance"]);
    hosted.stop().await.expect("stop");
}

#[tokio::test]
async fn a_malformed_envelope_fails_the_turn() {
    let dir = tempfile::tempdir().expect("tempdir");
    let socket = dir.path().join("craft.sock");
    let hosted = hosted(&socket, &["--bad-envelope"]).await.expect("start");
    let handle = &hosted.handles()[0];
    let error = handle
        .run(
            abi::text_message(flowcraft::proto::message::Role::User, "hi"),
            4,
        )
        .await
        .expect_err("bad envelope");
    assert_eq!(error.kind(), flowcraft::error::ErrorKind::Validation);
    hosted.stop().await.expect("stop");
}

#[tokio::test]
async fn an_undeclared_input_type_is_refused_before_sending() {
    let dir = tempfile::tempdir().expect("tempdir");
    let socket = dir.path().join("craft.sock");
    let hosted = hosted(&socket, &[]).await.expect("start");
    let handle = &hosted.handles()[0];
    let error = handle
        .run(abi::extension("not.declared", Vec::new()), 4)
        .await
        .expect_err("undeclared type");
    assert_eq!(error.kind(), flowcraft::error::ErrorKind::Validation);
    hosted.stop().await.expect("stop");
}
