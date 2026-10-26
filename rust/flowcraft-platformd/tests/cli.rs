#![allow(clippy::unwrap_used, clippy::expect_used)]

//! CLI and end-to-end tests for the daemon.

mod support;

use std::path::Path;
use std::process::Command;

use flowcraft::caps::workspace::Handle;
use flowcraft::kernel::{Config, Platform};
use flowcraft::resource::Deployment;

fn binary() -> &'static str {
    env!("CARGO_BIN_EXE_flowcraftd")
}

const CONFIG: &str = r#"
remote:
  mode: strict
  allow_targets: ["unix://__ROOT__/*"]
  endpoints:
    ws:
      target: "unix://__ROOT__/ws.sock"
      capabilities: ["workspace.Workspace/v1"]
grants:
  ttl_secs: 60
"#;

const DEPLOY: &str = r#"
version: v1
resources:
  local:
    kind: workspace.Workspace
    impl: local
    settings: { root: __ROOT__/data }
  ep:
    kind: workspace.Endpoint
    impl: grpc
    deps: { workspace: local }
    settings: { endpoint: ws }
  client:
    kind: workspace.Workspace
    impl: remote
    deps: { endpoints: endpoints }
    settings: { endpoint: ws, scope: { write: true } }
"#;

/// The in-process decorators, which only plan if the builtin registry has them.
const DECORATED: &str = r#"
version: v1
resources:
  local:
    kind: workspace.Workspace
    impl: local
    settings: { root: __ROOT__/data }
  policy:
    kind: workspace.Workspace
    impl: scoped
    deps: { workspace: local }
    settings: { allow_write: ["**"], mandatory_deny: ["**/.env"] }
  view:
    kind: workspace.Workspace
    impl: sub
    deps: { workspace: policy }
    settings: { prefix: run-1 }
"#;

fn write(path: &Path, text: &str) {
    std::fs::write(path, text).expect("write fixture");
}

fn fixture() -> (tempfile::TempDir, std::path::PathBuf, std::path::PathBuf) {
    let dir = tempfile::tempdir().expect("tempdir");
    let root = dir.path().display().to_string();
    let config = dir.path().join("platform.yaml");
    let deploy = dir.path().join("deploy.yaml");
    write(&config, &CONFIG.replace("__ROOT__", &root));
    write(&deploy, &DEPLOY.replace("__ROOT__", &root));
    (dir, config, deploy)
}

#[test]
fn check_accepts_a_valid_deployment() {
    let (_dir, config, deploy) = fixture();
    let output = support::isolated(Command::new(binary()))
        .args([
            "check",
            "--config",
            config.to_str().expect("path"),
            "--deploy",
            deploy.to_str().expect("path"),
            "--json",
        ])
        .output()
        .expect("run");
    assert!(
        output.status.success(),
        "stderr: {}",
        String::from_utf8_lossy(&output.stderr)
    );
    let report: serde_json::Value = serde_json::from_slice(&output.stdout).expect("json report");
    assert_eq!(report["ok"], true);
    assert_eq!(report["resources"], 3);
    assert_eq!(report["errors"].as_array().expect("errors").len(), 0);
}

/// The shipped examples are documents too, and nothing else parses them: a
/// stale settings key or dep edge would be invisible until someone ran the
/// demo. `check` is the gate that keeps them honest.
#[test]
fn every_example_deploy_checks() {
    let examples = Path::new(env!("CARGO_MANIFEST_DIR")).join("../examples");
    let mut checked = 0;
    for entry in std::fs::read_dir(&examples).expect("examples directory") {
        let directory = entry.expect("example entry").path();
        let config = directory.join("platform.yaml");
        let deploy = directory.join("deploy.yaml");
        if !config.is_file() || !deploy.is_file() {
            continue;
        }
        let output = support::isolated(Command::new(binary()))
            .args([
                "check",
                "--config",
                config.to_str().expect("path"),
                "--deploy",
                deploy.to_str().expect("path"),
                "--json",
            ])
            .output()
            .expect("run");
        assert!(
            output.status.success(),
            "{}: stderr: {}",
            directory.display(),
            String::from_utf8_lossy(&output.stderr)
        );
        let report: serde_json::Value =
            serde_json::from_slice(&output.stdout).expect("json report");
        assert_eq!(report["ok"], true, "{}: {report}", directory.display());
        checked += 1;
    }
    assert!(
        checked >= 2,
        "expected the workspace and capabilities examples, found {checked}"
    );
}

#[test]
fn check_accepts_the_workspace_decorators() {
    let dir = tempfile::tempdir().expect("tempdir");
    let root = dir.path().display().to_string();
    let config = dir.path().join("platform.yaml");
    let deploy = dir.path().join("decorated.yaml");
    write(&config, &CONFIG.replace("__ROOT__", &root));
    write(&deploy, &DECORATED.replace("__ROOT__", &root));
    let output = support::isolated(Command::new(binary()))
        .args([
            "check",
            "--config",
            config.to_str().expect("path"),
            "--deploy",
            deploy.to_str().expect("path"),
            "--json",
        ])
        .output()
        .expect("run");
    assert!(
        output.status.success(),
        "stderr: {}",
        String::from_utf8_lossy(&output.stderr)
    );
    let report: serde_json::Value = serde_json::from_slice(&output.stdout).expect("json report");
    assert_eq!(report["ok"], true, "{report}");
    assert_eq!(report["resources"], 3, "{report}");
}

#[test]
fn check_reports_every_plan_error_and_exits_two() {
    let (_dir, config, deploy) = fixture();
    // Two independent problems: an unknown kind and a dependency on nothing.
    write(
        &deploy,
        r#"
version: v1
resources:
  broken: { kind: "nope.Kind", impl: "x" }
  orphan:
    kind: workspace.Workspace
    impl: local
    settings: { root: /tmp }
    deps: { workspace: missing }
"#,
    );
    let output = support::isolated(Command::new(binary()))
        .args([
            "check",
            "--config",
            config.to_str().expect("path"),
            "--deploy",
            deploy.to_str().expect("path"),
            "--json",
        ])
        .output()
        .expect("run");
    assert_eq!(output.status.code(), Some(2));
    let report: serde_json::Value = serde_json::from_slice(&output.stdout).expect("json report");
    assert_eq!(report["ok"], false);
    let errors = report["errors"].as_array().expect("errors");
    assert_eq!(errors.len(), 2, "{errors:?}");
}

#[test]
fn a_successful_check_keeps_stderr_empty() {
    let (_dir, config, deploy) = fixture();
    let output = support::isolated(Command::new(binary()))
        .args([
            "check",
            "--config",
            config.to_str().expect("path"),
            "--deploy",
            deploy.to_str().expect("path"),
        ])
        .output()
        .expect("run");
    assert!(output.status.success());
    assert!(
        output.stderr.is_empty(),
        "an operator command must not chatter: {}",
        String::from_utf8_lossy(&output.stderr)
    );
}

#[test]
fn json_records_are_one_object_per_line_with_the_resource_fields() {
    let (_dir, config, deploy) = fixture();
    let output = support::isolated(Command::new(binary()))
        .args([
            "check",
            "-v",
            "--log-format",
            "json",
            "--config",
            config.to_str().expect("path"),
            "--deploy",
            deploy.to_str().expect("path"),
        ])
        .output()
        .expect("run");
    assert!(output.status.success());

    // The report stays on stdout; the diagnostics that wrap it go to stderr.
    assert!(
        String::from_utf8_lossy(&output.stdout).starts_with("ok:"),
        "stdout: {}",
        String::from_utf8_lossy(&output.stdout)
    );

    let stderr = String::from_utf8_lossy(&output.stderr);
    assert!(!stderr.is_empty(), "-v must emit records");
    for line in stderr.lines() {
        let record: serde_json::Value =
            serde_json::from_str(line).unwrap_or_else(|error| panic!("{error}: {line}"));
        assert_eq!(record["flowcraft.install.scope"], "machine");
        assert!(
            record["flowcraft.install"]
                .as_str()
                .is_some_and(|value| value.len() == 26),
            "{line}"
        );
        assert!(record["flowcraft.session"].is_string(), "{line}");
        assert!(record["level"].is_string(), "{line}");
        assert!(record["msg"].is_string(), "{line}");
    }
}

#[test]
fn a_bad_level_does_not_silence_the_daemon() {
    let (_dir, config, deploy) = fixture();
    let output = support::isolated(Command::new(binary()))
        .args([
            "check",
            "--log-level",
            "info,=oops",
            "--config",
            config.to_str().expect("path"),
            "--deploy",
            deploy.to_str().expect("path"),
        ])
        .output()
        .expect("run");
    assert!(output.status.success());
    let stderr = String::from_utf8_lossy(&output.stderr);
    assert!(
        stderr.contains("using the profile default"),
        "the fallback must be reported: {stderr}"
    );
    assert!(String::from_utf8_lossy(&output.stdout).starts_with("ok:"));
}

#[test]
fn an_invalid_log_section_is_a_configuration_error() {
    let (_dir, config, deploy) = fixture();
    let mut text = std::fs::read_to_string(&config).expect("config");
    text.push_str("log:\n  format: xml\n");
    write(&config, &text);
    let output = support::isolated(Command::new(binary()))
        .args([
            "check",
            "--config",
            config.to_str().expect("path"),
            "--deploy",
            deploy.to_str().expect("path"),
        ])
        .output()
        .expect("run");
    assert_eq!(output.status.code(), Some(2));
    let stderr = String::from_utf8_lossy(&output.stderr);
    assert!(stderr.contains("log.format"), "{stderr}");
}

#[test]
fn catalog_lists_the_builtin_providers() {
    let output = support::isolated(Command::new(binary()))
        .args(["catalog", "--json"])
        .output()
        .expect("run");
    assert!(output.status.success());
    let catalog: serde_json::Value = serde_json::from_slice(&output.stdout).expect("json");
    let kinds: Vec<&str> = catalog["entries"]
        .as_array()
        .expect("entries")
        .iter()
        .map(|entry| entry["kind"].as_str().expect("kind"))
        .collect();
    assert!(kinds.contains(&"workspace.Workspace"), "{kinds:?}");
    assert!(kinds.contains(&"workspace.Endpoint"), "{kinds:?}");
}

#[test]
fn version_is_a_single_line() {
    let output = support::isolated(Command::new(binary()))
        .arg("version")
        .output()
        .expect("run");
    assert!(output.status.success());
    let text = String::from_utf8_lossy(&output.stdout);
    assert!(text.trim_start().starts_with("flowcraftd "), "{text}");
}

#[test]
fn missing_deploy_flag_is_a_usage_error() {
    let output = support::isolated(Command::new(binary()))
        .arg("check")
        .output()
        .expect("run");
    assert_eq!(output.status.code(), Some(2));
    let stderr = String::from_utf8_lossy(&output.stderr);
    assert!(stderr.contains("--deploy"), "{stderr}");
}

#[tokio::test]
async fn the_assembly_builds_starts_and_serves_the_remote_capability() {
    let (dir, config_path, deploy_path) = fixture();
    let config = Config::parse(&std::fs::read(&config_path).expect("config")).expect("parse");
    let platform = Platform::new(config).expect("platform");
    let deployment: Deployment =
        flowcraft::resource::deployment(&std::fs::read(&deploy_path).expect("deploy"))
            .expect("decode");

    let built = flowcraft_platformd::assembly::build(&platform, deployment, Some(dir.path()))
        .await
        .expect("build");

    let client = built.get::<Handle>("client").expect("client handle");
    client
        .write("hello.txt", b"from the daemon")
        .await
        .expect("write");
    assert_eq!(
        client.read("hello.txt").await.expect("read"),
        b"from the daemon"
    );

    built.shutdown().await.expect("shutdown");
    platform.revoke_session(platform.session()).expect("revoke");
}

#[tokio::test]
async fn settings_resolve_from_the_deployment_secret_store() {
    let dir = tempfile::tempdir().expect("tempdir");
    let root = dir.path().join("data").display().to_string();
    let platform = Platform::new(Config::default()).expect("platform");
    let deployment: Deployment = flowcraft::resource::deployment(
        serde_json::to_vec(&serde_json::json!({
            "version": "v1",
            "resources": {
                "secrets": {
                    "kind": "secret.Store",
                    "impl": "memory",
                    "settings": {"values": {"demo/root": root}, "default": true}
                },
                "store": {
                    "kind": "workspace.Workspace",
                    "impl": "local",
                    "settings": {"root": "${secret:demo/root}"}
                }
            }
        }))
        .expect("json")
        .as_slice(),
    )
    .expect("document");

    let built = flowcraft_platformd::assembly::build(&platform, deployment, None)
        .await
        .expect("build");
    let store = built
        .get::<flowcraft::caps::workspace::Handle>("store")
        .expect("store");
    store.write("run-1", b"run-1").await.expect("save");
    assert!(
        std::path::Path::new(&root).join("run-1").exists(),
        "the resolved root must be where the workspace write landed"
    );
    built.shutdown().await.expect("shutdown");
}

#[tokio::test]
async fn an_absent_secret_fails_the_build() {
    let platform = Platform::new(Config::default()).expect("platform");
    let deployment: Deployment = flowcraft::resource::deployment(
        serde_json::to_vec(&serde_json::json!({
            "version": "v1",
            "resources": {
                "secrets": {
                    "kind": "secret.Store",
                    "impl": "memory",
                    "settings": {"default": true}
                },
                "store": {
                    "kind": "workspace.Workspace",
                    "impl": "local",
                    "settings": {"root": "${secret:demo/missing}"}
                }
            }
        }))
        .expect("json")
        .as_slice(),
    )
    .expect("document");
    let error = flowcraft_platformd::assembly::build(&platform, deployment, None)
        .await
        .expect_err("missing secret");
    assert_eq!(
        error.kind(),
        flowcraft::error::ErrorKind::NotFound,
        "{error}"
    );
    assert!(error.to_string().contains("demo/missing"), "{error}");
}

#[test]
fn check_rejects_an_unknown_endpoint_name() {
    let (_dir, config, deploy) = fixture();
    let text = std::fs::read_to_string(&deploy).expect("deploy");
    std::fs::write(&deploy, text.replace("endpoint: ws", "endpoint: nope")).expect("write");
    let output = support::isolated(Command::new(binary()))
        .args([
            "check",
            "--config",
            config.to_str().expect("path"),
            "--deploy",
            deploy.to_str().expect("path"),
            "--json",
        ])
        .output()
        .expect("run");
    assert_eq!(output.status.code(), Some(2));
    let report: serde_json::Value = serde_json::from_slice(&output.stdout).expect("json report");
    let errors = report["errors"].as_array().expect("errors");
    assert!(
        errors.iter().any(|error| error["path"]
            .as_str()
            .unwrap_or_default()
            .ends_with("settings.endpoint")),
        "{errors:?}"
    );
}

#[test]
fn check_rejects_a_capability_the_endpoint_does_not_serve() {
    let (_dir, config, deploy) = fixture();
    let text = std::fs::read_to_string(&deploy).expect("deploy");
    std::fs::write(
        &deploy,
        text.replace(
            "settings: { endpoint: ws }",
            "settings: { endpoint: ws, capability: sandbox.Runner/v1 }",
        ),
    )
    .expect("write");
    let output = support::isolated(Command::new(binary()))
        .args([
            "check",
            "--config",
            config.to_str().expect("path"),
            "--deploy",
            deploy.to_str().expect("path"),
            "--json",
        ])
        .output()
        .expect("run");
    assert_eq!(output.status.code(), Some(2));
    let report: serde_json::Value = serde_json::from_slice(&output.stdout).expect("json report");
    let rendered = report.to_string();
    assert!(rendered.contains("does not serve"), "{rendered}");
}

#[test]
fn check_merges_multiple_deploy_documents() {
    let (dir, config, deploy) = fixture();
    // A YAML overlay on top of the JSON base: later layers win.
    let overlay = dir.path().join("overlay.yaml");
    std::fs::write(
        &overlay,
        "entries: [local]\nresources:\n  client:\n    kind: workspace.Workspace\n    impl: remote\n    deps:\n      endpoints: endpoints\n    settings: {endpoint: ws, scope: {write: false}}\n",
    )
    .expect("overlay");
    let output = support::isolated(Command::new(binary()))
        .args([
            "check",
            "--config",
            config.to_str().expect("path"),
            "--deploy",
            deploy.to_str().expect("path"),
            "--deploy",
            overlay.to_str().expect("path"),
            "--json",
        ])
        .output()
        .expect("run");
    assert!(
        output.status.success(),
        "stderr: {}\nstdout: {}",
        String::from_utf8_lossy(&output.stderr),
        String::from_utf8_lossy(&output.stdout)
    );
    let report: serde_json::Value = serde_json::from_slice(&output.stdout).expect("json report");
    assert_eq!(report["ok"], true);
    assert_eq!(report["resources"], 3);
}

#[test]
fn subcommand_help_prints_usage() {
    let output = support::isolated(Command::new(binary()))
        .args(["ctl", "--help"])
        .output()
        .expect("run");
    assert!(output.status.success());
    let text = String::from_utf8_lossy(&output.stdout);
    assert!(text.contains("flowcraftd ctl"), "{text}");
}
