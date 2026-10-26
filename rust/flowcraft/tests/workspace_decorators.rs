#![allow(clippy::unwrap_used, clippy::expect_used)]

//! The decorators as resources: plan-time wiring, and which layer a rule names.
//!
//! [`conformance`](../conformance.rs) proves the decorators are transparent.
//! This file covers the two things that are not transparency: a document that
//! forgets a dependency or misspells a pattern fails before anything is built,
//! and a policy holds for a caller that arrives over the endpoint.

#[path = "workspace_common/mod.rs"]
mod common;

use std::sync::Arc;

use flowcraft::caps::workspace::contract::Workspace;
use flowcraft::caps::workspace::scoped::{Policy, ScopedWorkspace};
use flowcraft::caps::workspace::sub::SubWorkspace;
use flowcraft::caps::workspace::{Handle, local, scoped, sub};
use flowcraft::error::ErrorKind;
use flowcraft::resource::decode::deployment;
use flowcraft::resource::{Builder, Registry};
use serde_json::json;

/// A registry with only the in-process factories.
fn registry() -> Arc<Registry> {
    let mut registry = Registry::new();
    registry.register(local::LocalFactory).unwrap();
    registry.register(scoped::ScopedFactory).unwrap();
    registry.register(sub::SubFactory).unwrap();
    Arc::new(registry)
}

fn one(values: &[&str]) -> Vec<String> {
    values.iter().map(|value| (*value).to_owned()).collect()
}

#[tokio::test]
async fn a_decorator_without_its_workspace_dependency_fails_the_plan() {
    let document = deployment(
        serde_json::to_vec(&json!({
            "version": "v1",
            "resources": {
                "scoped": {
                    "kind": "workspace.Workspace",
                    "impl": "scoped",
                    "settings": {"allow_write": ["**"]}
                }
            }
        }))
        .unwrap()
        .as_slice(),
    )
    .unwrap();
    let error = Builder::new(registry())
        .deploy(document)
        .await
        .expect_err("the plan must reject a missing dependency");
    assert!(
        error.to_string().contains("workspace"),
        "the failure should name the missing edge: {error}"
    );
}

#[tokio::test]
async fn an_invalid_policy_pattern_fails_the_plan_without_io() {
    let dir = tempfile::tempdir().unwrap();
    let document = deployment(
        serde_json::to_vec(&json!({
            "version": "v1",
            "resources": {
                "local": {
                    "kind": "workspace.Workspace",
                    "impl": "local",
                    "settings": {"root": dir.path().join("data")}
                },
                "scoped": {
                    "kind": "workspace.Workspace",
                    "impl": "scoped",
                    "deps": {"workspace": "local"},
                    "settings": {"deny_read": ["a[b"]}
                }
            }
        }))
        .unwrap()
        .as_slice(),
    )
    .unwrap();
    let error = Builder::new(registry())
        .deploy(document)
        .await
        .expect_err("the plan must reject an invalid pattern");
    assert!(error.to_string().contains("invalid pattern"), "{error}");
}

#[tokio::test(flavor = "multi_thread")]
async fn a_policy_holds_for_a_caller_that_arrives_over_the_endpoint() {
    let stack = common::start().await;
    let remote = stack.handle("remote");

    // The stack wires `remote` to the endpoint serving the view, which sits on
    // the scoped provider. The mandatory-deny rule names `.env` at any depth, so
    // it still matches after the prefix is applied — and it applies to reads
    // too, which is why `exists` is a refusal rather than `false`.
    remote.write("notes.txt", b"ok").await.unwrap();
    assert_eq!(remote.read("notes.txt").await.unwrap(), b"ok");

    // The fixture's deny rule is spelled `**/secrets/**`, so it still matches
    // after the prefix is applied; writes are allowed, reads are not.
    remote.write("secrets/notes.txt", b"x").await.unwrap();
    assert_eq!(
        remote.read("secrets/notes.txt").await.unwrap_err().kind(),
        ErrorKind::Forbidden
    );
    let error = remote.write(".env", b"SECRET=1").await.unwrap_err();
    assert_eq!(error.kind(), ErrorKind::Forbidden);
    assert_eq!(
        remote.exists(".env").await.unwrap_err().kind(),
        ErrorKind::Forbidden
    );

    stack.built.shutdown().await.unwrap();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_rule_names_the_paths_the_layer_below_it_joins() {
    let stack = common::start().await;
    let local = stack.handle("local");

    // Policy *outside* the view: it sees the caller's names, so an allow list is
    // spelled without the prefix, and the escape is refused by the view that
    // joins it.
    let policy = Policy::new(&[], &one(&["**"]), &[]).unwrap();
    let view = SubWorkspace::new(local.clone(), "outer").unwrap();
    let composed = ScopedWorkspace::new(Handle::new(Arc::new(view)), policy);
    composed.write("file.txt", b"x").await.unwrap();
    assert_eq!(composed.read("file.txt").await.unwrap(), b"x");
    assert_eq!(local.read("outer/file.txt").await.unwrap(), b"x");
    assert_eq!(
        composed
            .write("../escape.txt", b"x")
            .await
            .unwrap_err()
            .kind(),
        ErrorKind::Forbidden
    );

    // Policy *inside* the view: the prefix is already applied by the time the
    // policy is asked, so a rule spelled for the unprefixed name stops matching.
    // That is not a bug to paper over — it is why the pattern and the layer have
    // to be read together (`**/secret/**` matches in both orders).
    let policy = Policy::new(&one(&["secret/**"]), &one(&["**"]), &[]).unwrap();
    let scoped = Handle::new(Arc::new(ScopedWorkspace::new(local, policy)));
    let view = SubWorkspace::new(scoped.clone(), "inner").unwrap();
    view.write("secret/file.txt", b"visible").await.unwrap();
    assert_eq!(view.read("secret/file.txt").await.unwrap(), b"visible");
    // The same provider, asked for the prefixed path, is not matched either…
    assert_eq!(
        scoped.read("inner/secret/file.txt").await.unwrap(),
        b"visible"
    );
    // …while the unprefixed name is refused before the provider is consulted.
    assert_eq!(
        scoped.read("secret/file.txt").await.unwrap_err().kind(),
        ErrorKind::Forbidden
    );

    stack.built.shutdown().await.unwrap();
}
