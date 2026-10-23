#![allow(clippy::unwrap_used, clippy::expect_used)]

//! Pins the crate dependency graph documented in `rust/README.md`.
//!
//! Rust compiles a whole crate at a time, so a stray import is invisible in a
//! review but shows up here as a new edge. Only normal dependencies count:
//! dev-dependencies are for tests and may point anywhere.

use std::collections::BTreeMap;
use std::process::Command;

fn graph() -> BTreeMap<String, Vec<String>> {
    let cargo = std::env::var("CARGO").unwrap_or_else(|_| "cargo".to_owned());
    let output = Command::new(cargo)
        .args(["metadata", "--no-deps", "--format-version", "1"])
        .current_dir(env!("CARGO_MANIFEST_DIR"))
        .output()
        .expect("cargo metadata runs");
    assert!(
        output.status.success(),
        "cargo metadata failed: {}",
        String::from_utf8_lossy(&output.stderr)
    );
    let value: serde_json::Value = serde_json::from_slice(&output.stdout).expect("metadata json");
    let mut graph = BTreeMap::new();
    for package in value["packages"].as_array().expect("packages") {
        let name = package["name"].as_str().expect("package name").to_owned();
        let mut deps: Vec<String> = package["dependencies"]
            .as_array()
            .expect("dependencies")
            .iter()
            // `kind` is null for a normal dependency; "dev"/"build" otherwise.
            .filter(|dep| dep["kind"].is_null())
            .filter_map(|dep| dep["name"].as_str())
            .filter(|dep| dep.starts_with("flowcraft"))
            .map(str::to_owned)
            .collect();
        deps.sort();
        graph.insert(name, deps);
    }
    graph
}

#[test]
fn leaves_stay_leaves() {
    let graph = graph();
    assert_eq!(graph["flowcraft-error"], Vec::<String>::new());
    assert_eq!(graph["flowcraft-proto"], Vec::<String>::new());
}

#[test]
fn search_depends_only_on_the_error_taxonomy() {
    let graph = graph();
    assert_eq!(graph["flowcraft-search"], vec!["flowcraft-error"]);
}

#[test]
fn telemetry_depends_only_on_the_error_taxonomy() {
    let graph = graph();
    assert_eq!(graph["flowcraft-telemetry"], vec!["flowcraft-error"]);
}

#[test]
fn resource_depends_on_the_taxonomy_and_the_log_vocabulary() {
    // Telemetry is below the composition kernel on purpose: the kernel, the
    // capabilities and the resources all report failures the same way, and that
    // way lives in one crate. It pulls no transport, by its own gate.
    let graph = graph();
    assert_eq!(
        graph["flowcraft-resource"],
        vec!["flowcraft-error", "flowcraft-telemetry"]
    );
}

#[test]
fn kernel_does_not_see_the_client_or_proto() {
    let graph = graph();
    let deps = &graph["flowcraft-kernel"];
    assert!(!deps.iter().any(|dep| dep == "flowcraft-client"));
    assert!(!deps.iter().any(|dep| dep == "flowcraft-proto"));
}

#[test]
fn client_sits_between_kernel_and_capabilities() {
    let graph = graph();
    let deps = &graph["flowcraft-client"];
    assert!(deps.iter().any(|dep| dep == "flowcraft-kernel"), "{deps:?}");
    assert!(deps.iter().any(|dep| dep == "flowcraft-proto"), "{deps:?}");
    // Telemetry is a leaf the client uses for trace propagation; it must not
    // pull in anything above the error taxonomy.
    assert_eq!(
        deps,
        &[
            "flowcraft-error",
            "flowcraft-kernel",
            "flowcraft-proto",
            "flowcraft-telemetry"
        ]
    );
}

#[test]
fn capabilities_depend_only_on_the_public_layers() {
    let allowed = [
        "flowcraft-error",
        "flowcraft-resource",
        "flowcraft-kernel",
        "flowcraft-client",
        "flowcraft-proto",
        "flowcraft-search",
    ];
    let graph = graph();
    for capability in [
        "flowcraft-cap-workspace",
        "flowcraft-cap-secret",
        "flowcraft-cap-sandbox",
        "flowcraft-cap-tool",
    ] {
        for dep in &graph[capability] {
            assert!(
                allowed.contains(&dep.as_str()),
                "{capability} depends on {dep}, which is not a public layer"
            );
        }
    }
}

#[test]
fn the_facade_reexports_every_crate_and_nobody_depends_on_it() {
    let graph = graph();
    let deps = &graph["flowcraft"];
    for layer in [
        "flowcraft-error",
        "flowcraft-telemetry",
        "flowcraft-proto",
        "flowcraft-resource",
        "flowcraft-kernel",
        "flowcraft-client",
        "flowcraft-search",
        "flowcraft-cap-workspace",
        "flowcraft-cap-secret",
        "flowcraft-cap-sandbox",
        "flowcraft-cap-tool",
    ] {
        assert!(
            deps.iter().any(|dep| dep == layer),
            "facade must re-export {layer}"
        );
    }
    for (name, deps) in &graph {
        if name == "flowcraft" {
            continue;
        }
        assert!(
            !deps.iter().any(|dep| dep == "flowcraft"),
            "{name} depends on the facade; that is a cycle (facade re-exports {name})"
        );
    }
}

#[test]
fn platformd_wires_every_layer() {
    let graph = graph();
    let deps = &graph["flowcraft-platformd"];
    for layer in [
        "flowcraft-error",
        "flowcraft-telemetry",
        "flowcraft-resource",
        "flowcraft-kernel",
        "flowcraft-client",
        "flowcraft-proto",
    ] {
        assert!(deps.iter().any(|dep| dep == layer), "missing {layer}");
    }
}
