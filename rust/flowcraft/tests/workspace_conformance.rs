// A test may reach the filesystem directly: it has no executor to starve.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::disallowed_methods)]

//! One contract suite, run over every expression of the contract.
//!
//! The four handles in [`common`] differ in everything except the contract:
//! a provider, a policy decorator, a prefix decorator, and a gRPC round trip.
//! Running the same assertions over all four is what makes "the decorators are
//! transparent" a fact instead of a claim — and it is the test shape the Go side
//! uses for its backends.

#[path = "workspace_common/mod.rs"]
mod common;

use flowcraft::caps::workspace::contract::Workspace;
use flowcraft::caps::workspace::helpers::{atomic_write, capped, glob};
use flowcraft::error::ErrorKind;

/// Everything the contract promises, asserted for one workspace.
async fn contract_suite(label: &str, workspace: &dyn Workspace) {
    let capabilities = workspace.capabilities().await.unwrap();
    assert!(capabilities.atomic_rename, "{label}: atomic rename");
    assert!(capabilities.read_after_write, "{label}: read after write");

    // Write creates parent directories; a read observes the write.
    let nested = format!("{label}/nested.txt");
    workspace.write(&nested, b"payload").await.unwrap();
    assert_eq!(
        workspace.read(&nested).await.unwrap(),
        b"payload",
        "{label}"
    );
    assert!(workspace.exists(&nested).await.unwrap(), "{label}");
    assert_eq!(workspace.stat(&nested).await.unwrap().size, 7, "{label}");
    assert_eq!(workspace.stat(&nested).await.unwrap().name, "nested.txt");

    // Append adds to the existing bytes.
    workspace.append(&nested, b"!").await.unwrap();
    assert_eq!(workspace.read(&nested).await.unwrap(), b"payload!");

    // Rename is the publication operation: the source stops existing.
    let published = format!("{label}.published");
    workspace.rename(&nested, &published).await.unwrap();
    assert!(!workspace.exists(&nested).await.unwrap());
    assert_eq!(workspace.read(&published).await.unwrap(), b"payload!");

    // List is sorted by name; a missing directory is empty, not an error.
    workspace.write(&format!("{label}/b"), b"b").await.unwrap();
    workspace.write(&format!("{label}/a"), b"a").await.unwrap();
    let names: Vec<String> = workspace
        .list(label)
        .await
        .unwrap()
        .into_iter()
        .map(|entry| entry.name)
        .collect();
    assert_eq!(names, vec!["a".to_owned(), "b".to_owned()], "{label}");
    assert!(
        workspace
            .list(&format!("{label}/missing"))
            .await
            .unwrap()
            .is_empty()
    );

    // Bounded reads refuse rather than truncate.
    assert_eq!(capped(workspace, &published, 8).await.unwrap().len(), 8);
    assert_eq!(
        capped(workspace, &published, 7).await.unwrap_err().kind(),
        ErrorKind::Validation,
        "{label}: cap"
    );

    // Helpers compose over the same contract.
    atomic_write(workspace, &format!("{label}/atomic"), b"x")
        .await
        .unwrap();
    assert_eq!(
        glob(workspace, &format!("{label}/*")).await.unwrap(),
        vec![
            format!("{label}/a"),
            format!("{label}/atomic"),
            format!("{label}/b")
        ],
        "{label}: glob"
    );

    // Delete is idempotent, and a directory is a validation error, not a
    // recursive removal.
    workspace.delete(&published).await.unwrap();
    workspace.delete(&published).await.unwrap();
    assert!(!workspace.exists(&published).await.unwrap());
    assert_eq!(
        workspace.delete(label).await.unwrap_err().kind(),
        ErrorKind::Validation,
        "{label}: delete a directory"
    );

    // RemoveAll is idempotent, and the root of the view is off limits.
    workspace.remove_all(label).await.unwrap();
    workspace.remove_all(label).await.unwrap();
    for root in ["", "."] {
        assert_eq!(
            workspace.remove_all(root).await.unwrap_err().kind(),
            ErrorKind::Forbidden,
            "{label}: remove {root:?}"
        );
    }

    // A path that climbs above the root is forbidden, not "not found".
    for path in ["..", "../escape", "/etc/passwd"] {
        assert_eq!(
            workspace.read(path).await.unwrap_err().kind(),
            ErrorKind::Forbidden,
            "{label}: {path}"
        );
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn every_expression_of_the_contract_behaves_the_same() {
    let stack = common::start().await;
    for label in ["local", "scoped", "view", "remote"] {
        let workspace = stack.handle(label);
        contract_suite(label, workspace.workspace().as_ref()).await;
    }
    stack.built.shutdown().await.unwrap();
}
