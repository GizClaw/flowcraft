// A test may reach the filesystem directly: it has no executor to starve.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::disallowed_methods)]

//! A craft's own output becomes attributed records.
//!
//! One test per binary: the pipeline is process-global and the assertion is on
//! what the daemon would actually write, read back out of the memory ring.

mod support;

use std::collections::BTreeMap;
use std::time::Duration;

use flowcraft::kernel::{Config, CraftConfig, Platform};
use flowcraft::telemetry::{Install, Profile, install};
use flowcraft_platformd::hosting::HostedCrafts;

fn daemon() -> &'static str {
    env!("CARGO_BIN_EXE_flowcraftd")
}

fn config(dir: &std::path::Path) -> Config {
    let socket = dir.join("craft.sock");
    Config {
        crafts: BTreeMap::from([(
            "demo".to_owned(),
            CraftConfig {
                argv: vec![
                    daemon().to_owned(),
                    "craft-stub".to_owned(),
                    "--socket".to_owned(),
                    socket.display().to_string(),
                    "--name".to_owned(),
                    "demo".to_owned(),
                ],
                target: format!("unix://{}", socket.display()),
                tls: None,
                token: String::new(),
                tenant: String::new(),
                grants: Vec::new(),
                // The child must not touch the identity of the machine
                // the test runs on.
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

#[tokio::test(flavor = "multi_thread")]
async fn a_spawned_crafts_output_is_attributed() {
    let guard = install(
        Install::new(Profile::Service)
            .with_directives("info")
            .with_ring(256 * 1024),
    )
    .expect("install");

    let dir = tempfile::tempdir().expect("tempdir");
    let config = config(dir.path());
    config.validate().expect("valid config");
    let platform = std::sync::Arc::new(Platform::new(config).expect("platform"));

    let hosted = HostedCrafts::start(platform.clone(), &platform.config().crafts)
        .await
        .expect("host");

    // The child logs as soon as it binds; the pump reads it a moment later.
    let mut dump = String::new();
    for _ in 0..100 {
        dump = guard.ring_dump().unwrap_or_default();
        if dump.contains("craft stub listening") {
            break;
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
    }

    assert!(
        dump.contains("craft stub listening"),
        "the craft's own line must reach the daemon's log:\n{dump}"
    );
    assert!(dump.contains("flowcraft.craft=demo"), "{dump}");
    assert!(dump.contains("flowcraft.stream=stdout"), "{dump}");
    assert!(
        !dump.contains("flowcraft.craft=\"demo\""),
        "the craft name is a plain value, not a quoted one:\n{dump}"
    );

    hosted.stop().await.expect("stop");
}
