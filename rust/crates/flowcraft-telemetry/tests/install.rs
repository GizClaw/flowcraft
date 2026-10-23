#![allow(clippy::unwrap_used, clippy::expect_used)]

//! Installing the pipeline is process-global, so this file holds exactly one
//! test: an integration test binary is its own process, and a second `install`
//! here would fail by design.

use flowcraft_error::ErrorKind;
use flowcraft_telemetry::{Format, Guard, Install, Profile, Resource, install};

#[test]
fn installs_once_and_reloads_the_filter() {
    let resource = Resource {
        service: "flowcraftd-test".to_owned(),
        version: "0.0.0".to_owned(),
        install: Some("01J8Z5Q2K7M4N9P3R6T0V2X4B6".to_owned()),
        install_scope: Some("machine".to_owned()),
        install_epoch: 1,
        session: "s-test".to_owned(),
        app: None,
        principal: None,
    };
    let dir = tempfile::tempdir().expect("tempdir");
    let log = dir.path().join("flowcraftd.log");
    let guard: Guard = install(
        Install::new(Profile::Service)
            .with_format(Format::Json)
            .with_directives("info")
            .with_resource(resource)
            .with_ring(64 * 1024)
            .with_file(flowcraft_telemetry::sinks::file::Options::new(&log)),
    )
    .expect("first install wins");
    assert!(guard.is_installed());

    // The host reads its configuration after the pipeline exists; the level
    // must be replaceable without a restart.
    guard.set_directives("debug").expect("reload");
    assert_eq!(guard.directives().as_deref(), Some("debug"));

    // Emitting has to reach the sink with the resource fields attached; run
    // this test with `--nocapture` to see the JSON the host would ship.
    tracing::info!(flowcraft.craft = "alpha", "installed");
    flowcraft_telemetry::logerr!(
        warn,
        flowcraft_error::Error::not_available("no collector"),
        "export failed",
        flowcraft.craft = "alpha"
    );

    guard
        .set_directives("flowcraft_telemetry=warn")
        .expect("reload with a target");
    assert!(guard.directives().unwrap().contains("flowcraft_telemetry"));

    // A bad directive must be rejected, and must not disturb what is installed.
    let error = guard.set_directives("info,=trace").expect_err("invalid");
    assert_eq!(error.kind(), ErrorKind::Validation);
    assert_eq!(
        guard.directives().as_deref(),
        Some("flowcraft_telemetry=warn")
    );

    // A second install must be refused rather than silently stealing the first.
    let second = install(Install::new(Profile::Cli));
    let error = second.expect_err("second install must fail");
    assert_eq!(error.kind(), ErrorKind::NotAvailable);

    // An error spills the ring next to the log, so the window that explains it
    // survives the process. The filter has to let the record through first.
    guard.set_directives("info").expect("widen");
    flowcraft_telemetry::logerr!(
        error,
        flowcraft_error::Error::internal("boom"),
        "something broke"
    );
    guard.flush();
    let spill = std::path::PathBuf::from(format!("{}.last-error", log.display()));
    assert!(spill.exists(), "expected {}", spill.display());
    let dumped = std::fs::read_to_string(&spill).expect("spill");
    assert!(dumped.contains("something broke"), "{dumped}");
    assert!(
        dumped.contains("installed"),
        "the window around it too:\n{dumped}"
    );

    guard.shutdown();
}
