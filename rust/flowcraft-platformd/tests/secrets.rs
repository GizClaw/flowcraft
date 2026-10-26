#![allow(clippy::unwrap_used, clippy::expect_used)]

//! Deployment-secret assembly: store ids, the default store, lazy references,
//! the encrypted backend, and host-injected stores.

use std::sync::Arc;

use flowcraft::caps::secret::{Handle as SecretHandle, Manager, MemoryStore, Store as SecretStore};
use flowcraft::kernel::{Config, Platform};
use flowcraft::resource::{Deployment, resolver::SecretStore as ResourceSecretStore};
use flowcraft_platformd::assembly::{BuildOptions, build, build_with};
use serde_json::json;

fn deployment(document: serde_json::Value) -> Deployment {
    flowcraft::resource::deployment(serde_json::to_vec(&document).unwrap().as_slice())
        .expect("document")
}

fn platform() -> Platform {
    Platform::new(Config::default()).expect("platform")
}

#[tokio::test]
async fn references_route_by_store_id_and_by_default() {
    let dir = tempfile::tempdir().unwrap();
    let team = dir.path().join("team").display().to_string();
    let shared = dir.path().join("shared").display().to_string();

    let built = build(
        &platform(),
        deployment(json!({
            "version": "v1",
            "resources": {
                "primary": {
                    "kind": "secret.Store",
                    "impl": "memory",
                    "settings": {
                        "values": {"root": team},
                        "id": "team",
                        "default": true
                    }
                },
                "secondary": {
                    "kind": "secret.Store",
                    "impl": "memory",
                    "settings": {"values": {"root": shared}, "id": "shared"}
                },
                "by-default": {
                    "kind": "workspace.Workspace",
                    "impl": "local",
                    "settings": {"root": "${secret:root}"}
                },
                "by-id": {
                    "kind": "workspace.Workspace",
                    "impl": "local",
                    "settings": {"root": "${secret:shared.root}"}
                }
            }
        })),
        None,
    )
    .await
    .expect("build");

    for (name, expected) in [("by-default", &team), ("by-id", &shared)] {
        let store = built
            .get::<flowcraft::caps::workspace::Handle>(name)
            .expect("store");
        store.write("run-1", b"run-1").await.expect("save");
        assert!(
            std::path::Path::new(expected).join("run-1").exists(),
            "{name} must resolve to {expected}"
        );
    }
    built.shutdown().await.unwrap();
}

#[tokio::test]
async fn a_second_default_store_fails_the_build() {
    let error = build(
        &platform(),
        deployment(json!({
            "version": "v1",
            "resources": {
                "a": {"kind": "secret.Store", "impl": "memory", "settings": {"default": true}},
                "b": {"kind": "secret.Store", "impl": "memory", "settings": {"default": true}},
                "store": {
                    "kind": "workspace.Workspace",
                    "impl": "local",
                    "settings": {"root": "${secret:x}"}
                }
            }
        })),
        None,
    )
    .await
    .expect_err("two defaults");
    assert_eq!(error.kind(), flowcraft::error::ErrorKind::Conflict);
    assert!(error.to_string().contains("default"), "{error}");
}

#[tokio::test]
async fn a_duplicate_store_id_fails_the_build() {
    let error = build(
        &platform(),
        deployment(json!({
            "version": "v1",
            "resources": {
                "a": {
                    "kind": "secret.Store", "impl": "memory",
                    "settings": {"id": "same", "default": true}
                },
                "b": {
                    "kind": "secret.Store", "impl": "memory",
                    "settings": {"id": "same"}
                }
            }
        })),
        None,
    )
    .await
    .expect_err("duplicate id");
    assert_eq!(error.kind(), flowcraft::error::ErrorKind::Conflict);
    assert!(error.to_string().contains("same"), "{error}");
}

#[tokio::test]
async fn a_name_only_reference_needs_a_default_store() {
    let error = build(
        &platform(),
        deployment(json!({
            "version": "v1",
            "resources": {
                "only": {
                    "kind": "secret.Store", "impl": "memory",
                    "settings": {"id": "team", "values": {"root": "/tmp/x"}}
                },
                "store": {
                    "kind": "workspace.Workspace",
                    "impl": "local",
                    "settings": {"root": "${secret:root}"}
                }
            }
        })),
        None,
    )
    .await
    .expect_err("no default store");
    assert_eq!(error.kind(), flowcraft::error::ErrorKind::Validation);
    assert!(error.to_string().contains("default"), "{error}");
}

#[tokio::test]
async fn every_reference_is_checked_against_the_declared_stores() {
    let error = build(
        &platform(),
        deployment(json!({
            "version": "v1",
            "resources": {
                "only": {
                    "kind": "secret.Store", "impl": "memory",
                    "settings": {"id": "team", "default": true, "values": {"root": "/tmp/x"}}
                },
                "store": {
                    "kind": "workspace.Workspace",
                    "impl": "local",
                    "settings": {"root": "${secret:other.root}"}
                }
            }
        })),
        None,
    )
    .await
    .expect_err("unknown store");
    // `other` is not a store id, so the whole path is a name in the default
    // store: it resolves, and the workspace then fails on the value.
    assert!(
        error.to_string().contains("other.root"),
        "the name must be reported: {error}"
    );
}

#[tokio::test]
async fn the_keychain_serves_lazy_references_and_stays_sealed() {
    let secrets_dir = tempfile::tempdir().unwrap();
    let stores = tempfile::tempdir().unwrap();
    let root = stores.path().join("stores").display().to_string();

    // Write the secret the way an app-side settings page would.
    let manager = Manager::open(secrets_dir.path());
    assert!(manager.available());
    manager.set("demo/root", &root).await.unwrap();

    let built = build(
        &platform(),
        deployment(json!({
            "version": "v1",
            "resources": {
                "secrets": {
                    "kind": "secret.Store",
                    "impl": "keychain",
                    "settings": {
                        "dir": secrets_dir.path().display().to_string(),
                        "default": true
                    }
                },
                "store": {
                    "kind": "workspace.Workspace",
                    "impl": "local",
                    "settings": {"root": "${secret:demo/root}"}
                }
            }
        })),
        None,
    )
    .await
    .expect("build");

    let store = built
        .get::<flowcraft::caps::workspace::Handle>("store")
        .expect("store");
    store.write("run-1", b"run-1").await.expect("save");
    assert!(std::path::Path::new(&root).join("run-1").exists());

    // The resolved value went into the store, not into the process's settings:
    // nothing in the secret directory holds the plaintext path.
    for entry in std::fs::read_dir(secrets_dir.path()).unwrap() {
        let entry = entry.unwrap();
        if entry.file_type().unwrap().is_file() {
            let bytes = std::fs::read(entry.path()).unwrap();
            assert!(
                !bytes
                    .windows(root.len())
                    .any(|window| window == root.as_bytes()),
                "{} holds the plaintext secret",
                entry.path().display()
            );
        }
    }
    built.shutdown().await.unwrap();
}

#[tokio::test]
async fn a_host_injected_store_is_the_default() {
    let stores = tempfile::tempdir().unwrap();
    let root = stores.path().join("host").display().to_string();
    let host: Arc<dyn SecretStore> =
        Arc::new(MemoryStore::new([("demo/root".to_owned(), root.clone())]));
    let handle = Arc::new(SecretHandle::new(host));
    let injected: Arc<dyn ResourceSecretStore> = handle;

    let built = build_with(
        &platform(),
        deployment(json!({
            "version": "v1",
            "resources": {
                "store": {
                    "kind": "workspace.Workspace",
                    "impl": "local",
                    "settings": {"root": "${secret:demo/root}"}
                }
            }
        })),
        None,
        &BuildOptions::default().with_default_secret("process", injected),
    )
    .await
    .expect("build with an injected store");

    let store = built
        .get::<flowcraft::caps::workspace::Handle>("store")
        .expect("store");
    store.write("run-1", b"run-1").await.expect("save");
    assert!(std::path::Path::new(&root).join("run-1").exists());
    built.shutdown().await.unwrap();
}

#[tokio::test]
async fn an_injected_default_conflicts_with_a_document_default() {
    let host: Arc<dyn SecretStore> = Arc::new(MemoryStore::new([("x".to_owned(), "y".to_owned())]));
    let injected: Arc<dyn ResourceSecretStore> = Arc::new(SecretHandle::new(host));
    let error = build_with(
        &platform(),
        deployment(json!({
            "version": "v1",
            "resources": {
                "secrets": {"kind": "secret.Store", "impl": "memory", "settings": {"default": true}}
            }
        })),
        None,
        &BuildOptions::default().with_default_secret("process", injected),
    )
    .await
    .expect_err("two defaults");
    assert_eq!(error.kind(), flowcraft::error::ErrorKind::Conflict);
}

#[tokio::test]
async fn a_missing_secret_still_fails_the_build_that_needs_it() {
    let error = build(
        &platform(),
        deployment(json!({
            "version": "v1",
            "resources": {
                "secrets": {"kind": "secret.Store", "impl": "memory", "settings": {"default": true}},
                "store": {
                    "kind": "workspace.Workspace",
                    "impl": "local",
                    "settings": {"root": "${secret:absent}"}
                }
            }
        })),
        None,
    )
    .await
    .expect_err("absent secret");
    assert_eq!(error.kind(), flowcraft::error::ErrorKind::NotFound);
    assert!(error.to_string().contains("absent"), "{error}");
}

#[tokio::test]
async fn the_file_provider_serves_one_file_per_secret() {
    let secrets_dir = tempfile::tempdir().unwrap();
    let stores = tempfile::tempdir().unwrap();
    let root = stores.path().join("from-file");
    std::fs::create_dir_all(&root).unwrap();
    std::fs::write(
        secrets_dir.path().join("workspace-root"),
        format!("{}\n", root.display()),
    )
    .unwrap();

    let built = build(
        &platform(),
        deployment(json!({
            "version": "v1",
            "resources": {
                "secrets": {
                    "kind": "secret.Store",
                    "impl": "file",
                    "settings": {
                        "base": secrets_dir.path().display().to_string(),
                        "default": true
                    }
                },
                "store": {
                    "kind": "workspace.Workspace",
                    "impl": "local",
                    "settings": {"root": "${secret:workspace-root}"}
                }
            }
        })),
        None,
    )
    .await
    .expect("build");

    let store = built
        .get::<flowcraft::caps::workspace::Handle>("store")
        .expect("store");
    store.write("run-1", b"run-1").await.expect("save");
    assert!(root.join("run-1").exists());
    built.shutdown().await.unwrap();
}
