#![allow(clippy::unwrap_used, clippy::expect_used)]

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
