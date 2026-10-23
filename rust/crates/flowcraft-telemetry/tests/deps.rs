#![allow(clippy::unwrap_used, clippy::expect_used)]

//! The pipeline has to stay cheap enough for a client binary.
//!
//! Binary size is a lagging indicator and hard to assert in a test. The
//! dependency closure is the leading indicator, and it is the thing that is
//! actually under our control: an async runtime or an HTTP stack in the default
//! build would be megabytes in every binary that links the platform, including
//! the one shipped inside a desktop app.
//!
//! The check runs `cargo tree` for this package with explicit features, so it
//! reflects what a build with those features really pulls — not what the whole
//! workspace happens to unify to.

use std::process::Command;

fn tree(features: &[&str]) -> String {
    let cargo = std::env::var("CARGO").unwrap_or_else(|_| "cargo".to_owned());
    let output = Command::new(cargo)
        .args([
            "tree",
            "-p",
            "flowcraft-telemetry",
            "--edges",
            "normal",
            "--prefix",
            "none",
            "--offline",
        ])
        .args(features)
        .current_dir(env!("CARGO_MANIFEST_DIR"))
        .output()
        .expect("cargo tree runs");
    assert!(
        output.status.success(),
        "cargo tree failed: {}",
        String::from_utf8_lossy(&output.stderr)
    );
    String::from_utf8(output.stdout).expect("utf-8")
}

/// The crate names in a `cargo tree` listing.
fn names(tree: &str) -> Vec<String> {
    tree.lines()
        .filter_map(|line| line.split_whitespace().next())
        .map(str::to_owned)
        .collect()
}

#[test]
fn the_default_build_carries_no_runtime_and_no_transport() {
    let names = names(&tree(&["--no-default-features", "--features", "otel"]));
    for forbidden in [
        "tokio",
        "reqwest",
        "hyper",
        "hyper-util",
        "opentelemetry-otlp",
        "opentelemetry-appender-tracing",
    ] {
        assert!(
            !names.iter().any(|name| name == forbidden),
            "the default pipeline must not pull {forbidden}"
        );
    }
}

#[test]
fn the_otlp_feature_is_where_the_transport_lives() {
    // The other half of the gate: if this stops being true, the feature has been
    // wired to the wrong half of the build.
    let names = names(&tree(&["--features", "otlp"]));
    for required in ["opentelemetry-otlp", "opentelemetry-appender-tracing"] {
        assert!(
            names.iter().any(|name| name == required),
            "the otlp feature must bring in {required}"
        );
    }
}

#[test]
fn the_pipeline_depends_on_the_error_taxonomy_only() {
    let names = names(&tree(&["--no-default-features", "--features", "otel"]));
    let internal: Vec<&String> = names
        .iter()
        .filter(|name| name.starts_with("flowcraft-") && *name != "flowcraft-telemetry")
        .collect();
    assert_eq!(internal, vec!["flowcraft-error"], "{internal:?}");
}
