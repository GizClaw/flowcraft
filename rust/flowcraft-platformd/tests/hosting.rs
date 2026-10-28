//! These spawn crafts on unix sockets and a POSIX shell; a Windows host has
//! neither in this crate yet, so the file is unix-only rather than untested.
#![cfg(unix)]
// A test may reach the filesystem directly: it has no executor to starve.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::disallowed_methods)]

//! Hosted crafts: config validation, grant minting, and process lifecycle.

mod support;

use std::collections::BTreeMap;
use std::sync::Arc;
use std::time::Duration;

use flowcraft::kernel::{
    Config, CraftConfig, CraftGrant, EndpointConfig, Mode, Platform, RemoteConfig,
};
use flowcraft_platformd::hosting::HostedCrafts;

fn binary() -> &'static str {
    env!("CARGO_BIN_EXE_flowcraftd")
}

fn config(dir: &std::path::Path, argv: Vec<String>) -> Config {
    let craft_socket = dir.join("craft.sock");
    Config {
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
        crafts: BTreeMap::from([(
            "demo".to_owned(),
            CraftConfig {
                argv,
                target: format!("unix://{}", craft_socket.display()),
                tls: None,
                token: String::new(),
                tenant: String::new(),
                grants: vec![CraftGrant {
                    capability: "workspace.Workspace/v1".to_owned(),
                    endpoint: "ws".to_owned(),
                    scope: serde_json::json!({"write": true}),
                }],
                env: BTreeMap::from([support::identity_env()]),
                ready_timeout_secs: 10,
                supervisor: "exec".to_owned(),
                unit: String::new(),
                systemctl: String::new(),
            },
        )]),
        ..Config::default()
    }
}

fn stub_argv(dir: &std::path::Path) -> Vec<String> {
    vec![
        binary().to_owned(),
        "craft-stub".to_owned(),
        "--socket".to_owned(),
        dir.join("craft.sock").display().to_string(),
        "--name".to_owned(),
        "demo".to_owned(),
    ]
}

#[tokio::test]
async fn hosts_a_craft_and_hands_it_grants() {
    let dir = tempfile::tempdir().expect("tempdir");
    let config = config(dir.path(), stub_argv(dir.path()));
    config.validate().expect("valid config");
    let platform = Arc::new(Platform::new(config).expect("platform"));

    let hosted = HostedCrafts::start(platform.clone(), &platform.config().crafts)
        .await
        .expect("host");
    assert_eq!(hosted.names(), vec!["demo"]);
    assert_eq!(hosted.len(), 1);

    let handle = &hosted.handles()[0];
    assert_eq!(handle.info().name, "demo");
    assert_eq!(handle.session(), "stub-session");
    assert_eq!(handle.grants().len(), 1);
    let grant = &handle.grants()[0];
    assert_eq!(grant.capability, "workspace.Workspace/v1");
    assert!(grant.endpoint.ends_with("ws.sock"), "{grant:?}");
    assert!(!grant.token.is_empty());
    assert!(grant.expires_at_unix > 0);

    let events = handle
        .run(
            flowcraft_platformd::abi::text_message(flowcraft::proto::message::Role::User, "hi"),
            4,
        )
        .await
        .expect("run");
    assert_eq!(events.last().expect("event").r#type, "done");

    hosted.stop().await.expect("stop");
    tokio::time::sleep(Duration::from_millis(300)).await;
    assert!(
        tokio::net::UnixStream::connect(dir.path().join("craft.sock"))
            .await
            .is_err()
    );
}

#[tokio::test]
async fn a_craft_that_cannot_start_fails_the_boot() {
    let dir = tempfile::tempdir().expect("tempdir");
    let config = config(
        dir.path(),
        vec!["/definitely/not/a/flowcraft-craft".to_owned()],
    );
    config.validate().expect("valid config");
    let platform = Arc::new(Platform::new(config).expect("platform"));
    let error = HostedCrafts::start(platform.clone(), &platform.config().crafts)
        .await
        .expect_err("cannot start");
    assert_eq!(error.kind(), flowcraft::error::ErrorKind::NotAvailable);
    assert!(error.to_string().contains("crafts.demo"), "{error}");
}

#[tokio::test]
async fn a_craft_that_never_listens_fails_the_boot() {
    let dir = tempfile::tempdir().expect("tempdir");
    let mut config = config(dir.path(), vec!["sleep".to_owned(), "30".to_owned()]);
    config
        .crafts
        .get_mut("demo")
        .expect("demo")
        .ready_timeout_secs = 1;
    config.validate().expect("valid config");
    let platform = Arc::new(Platform::new(config).expect("platform"));
    let error = HostedCrafts::start(platform.clone(), &platform.config().crafts)
        .await
        .expect_err("never listens");
    assert_eq!(error.kind(), flowcraft::error::ErrorKind::NotAvailable);
}

#[tokio::test]
async fn an_external_craft_is_connected_but_not_owned() {
    let dir = tempfile::tempdir().expect("tempdir");
    let socket = dir.path().join("craft.sock");
    // Someone else launches the craft.
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
    for _ in 0..250 {
        if tokio::net::UnixStream::connect(&socket).await.is_ok() {
            break;
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    }

    let mut config = config(dir.path(), Vec::new());
    let craft = config.crafts.get_mut("demo").expect("demo");
    craft.supervisor = "external".to_owned();
    craft.target = format!("unix://{}", socket.display());
    config.validate().expect("valid config");
    let platform = Arc::new(Platform::new(config).expect("platform"));

    let hosted = HostedCrafts::start(platform.clone(), &platform.config().crafts)
        .await
        .expect("host");
    assert_eq!(hosted.names(), vec!["demo"]);
    hosted.stop().await.expect("stop");

    // The platform only closed its session; the process is still ours.
    assert!(child.try_wait().expect("try_wait").is_none());
    let _ = child.kill().await;
}

#[tokio::test]
async fn a_systemd_craft_is_started_and_stopped_through_its_unit() {
    use std::os::unix::fs::PermissionsExt;

    let dir = tempfile::tempdir().expect("tempdir");
    let socket = dir.path().join("craft.sock");
    let pid_file = dir.path().join("unit.pid");
    let calls = dir.path().join("calls.log");
    let shim = dir.path().join("systemctl");
    let (identity_env, identity) = support::identity_env();
    let script = format!(
        "#!/bin/sh\n\nexport {identity_env}=\"{identity}\"\necho \"$@\" >> \"{calls}\"\ncase \"$1\" in\n  start)\n    \"{bin}\" craft-stub --socket \"{sock}\" --name demo >/dev/null 2>&1 &\n    echo $! > \"{pid}\"\n    ;;\n  stop)\n    if [ -f \"{pid}\" ]; then kill \"$(cat \"{pid}\")\" 2>/dev/null; rm -f \"{pid}\"; fi\n    ;;\n  *)\n    echo \"unexpected: $*\" >&2; exit 2 ;;\nesac\n",
        calls = calls.display(),
        bin = binary(),
        sock = socket.display(),
        pid = pid_file.display(),
        identity_env = identity_env,
        identity = identity,
    );
    std::fs::write(&shim, script).expect("shim");
    std::fs::set_permissions(&shim, std::fs::Permissions::from_mode(0o755)).expect("chmod");

    let mut config = config(dir.path(), Vec::new());
    let craft = config.crafts.get_mut("demo").expect("demo");
    craft.supervisor = "systemd".to_owned();
    craft.target = format!("unix://{}", socket.display());
    craft.systemctl = shim.display().to_string();
    config.validate().expect("valid config");
    let platform = Arc::new(Platform::new(config).expect("platform"));

    let hosted = HostedCrafts::start(platform.clone(), &platform.config().crafts)
        .await
        .expect("host");
    assert_eq!(hosted.names(), vec!["demo"]);
    assert_eq!(hosted.handles()[0].unit(), Some("flowcraft-demo.service"));
    let input = hosted.handles()[0];
    let events = input
        .run(
            flowcraft_platformd::abi::text_message(flowcraft::proto::message::Role::User, "hi"),
            4,
        )
        .await
        .expect("run");
    assert_eq!(events.last().expect("event").r#type, "done");

    hosted.stop().await.expect("stop");
    tokio::time::sleep(Duration::from_millis(300)).await;
    assert!(
        tokio::net::UnixStream::connect(&socket).await.is_err(),
        "the unit should have been stopped"
    );
    let log = std::fs::read_to_string(&calls).expect("calls");
    assert!(log.contains("start flowcraft-demo.service"), "{log}");
    assert!(log.contains("stop flowcraft-demo.service"), "{log}");
}

#[tokio::test]
async fn hosted_crafts_renew_their_grants_before_they_expire() {
    let dir = tempfile::tempdir().expect("tempdir");
    let log = dir.path().join("renew.log");
    let argv = vec![
        binary().to_owned(),
        "craft-stub".to_owned(),
        "--socket".to_owned(),
        dir.path().join("craft.sock").display().to_string(),
        "--name".to_owned(),
        "demo".to_owned(),
        "--renew-log".to_owned(),
        log.display().to_string(),
    ];
    let mut config = config(dir.path(), argv);
    // A one-second lease makes the renewal loop tick every second.
    config.grants.ttl_secs = 1;
    config.validate().expect("valid config");
    let platform = Arc::new(Platform::new(config).expect("platform"));

    let hosted = HostedCrafts::start(platform.clone(), &platform.config().crafts)
        .await
        .expect("host");
    tokio::time::sleep(Duration::from_millis(2600)).await;
    let text = std::fs::read_to_string(&log).unwrap_or_default();
    assert!(text.contains("renew"), "renewal log: {text:?}");
    hosted.stop().await.expect("stop");
}
