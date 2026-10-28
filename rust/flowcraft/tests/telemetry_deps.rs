// A test may reach the filesystem directly: it has no executor to starve.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::disallowed_methods)]

//! The pipeline has to stay cheap enough for a client binary.
//!
//! Binary size is a lagging indicator and hard to assert in a test. The
//! dependency closure is the leading indicator, and it is the thing that is
//! actually under our control: an async runtime or an HTTP stack in the build a
//! client links would be megabytes, including the binary shipped inside a
//! desktop app.
//!
//! The check runs `cargo tree` for this package with explicit features, so it
//! reflects what a build with *those* features really pulls — not what the whole
//! workspace happens to unify to. Since every layer is a module of one crate
//! now, this is the only place that distinction can be asserted: `dep:` entries
//! in the feature table are the crate boundary.
//!
//! It runs **without** `--offline` on purpose: the `otlp` closure is one the
//! build deliberately does not use, so on a cold cache its manifests are not
//! there and an offline resolution fails with "failed to download". Fetching is a
//! no-op once cached; opting out of resolution is what this test is for, not
//! opting out of the network.

use std::process::Command;

fn tree(features: &[&str]) -> String {
    let cargo = std::env::var("CARGO").unwrap_or_else(|_| "cargo".to_owned());
    let output = Command::new(cargo)
        .args([
            "tree",
            "-p",
            "flowcraft",
            "--edges",
            "normal",
            "--prefix",
            "none",
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
fn the_otel_build_carries_no_runtime_and_no_transport() {
    let names = names(&tree(&["--no-default-features", "--features", "otel"]));
    for forbidden in [
        "tokio",
        "reqwest",
        "hyper",
        "hyper-util",
        "opentelemetry-otlp",
        "opentelemetry-appender-tracing",
        "tonic",
        "saphyr",
        "nix",
        "cap-std",
        "rmcp",
    ] {
        assert!(
            !names.iter().any(|name| name == forbidden),
            "the telemetry build must not pull {forbidden}"
        );
    }
}

#[test]
fn the_otlp_feature_is_where_the_transport_lives() {
    // The other half of the gate: if this stops being true, the feature has been
    // wired to the wrong half of the build.
    let names = names(&tree(&["--no-default-features", "--features", "otlp"]));
    for required in ["opentelemetry-otlp", "opentelemetry-appender-tracing"] {
        assert!(
            names.iter().any(|name| name == required),
            "the otlp feature must bring in {required}"
        );
    }
}

#[test]
fn the_pipeline_is_the_taxonomy_and_the_tracing_stack_only() {
    // Every other layer lives in this crate, so "depends on nothing above the
    // error taxonomy" can only be asserted on the *external* closure: the
    // tracing stack, its proc macros and the compatibility shims they pull.
    let names = names(&tree(&["--no-default-features", "--features", "otel"]));
    for required in ["thiserror", "tracing", "tracing-subscriber"] {
        assert!(
            names.iter().any(|name| name == required),
            "the pipeline needs {required}"
        );
    }
    let flowcraft: Vec<&String> = names
        .iter()
        .filter(|name| name.starts_with("flowcraft"))
        .collect();
    assert_eq!(
        flowcraft,
        vec!["flowcraft"],
        "no other platform crate exists"
    );
}
