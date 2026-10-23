//! The local provider: one file per execution id.
//!
//! Writes are atomic (temporary file in the same directory, then rename), so a
//! reader never observes a half-written checkpoint. Execution ids are
//! restricted to a conservative alphabet and rejected if they would escape the
//! root.

use std::path::{Path, PathBuf};

use async_trait::async_trait;
use flowcraft_error::{Error, Result};
use flowcraft_resource::Kind;
use flowcraft_resource::{BoxFuture, BuildCtx, Factory, Phase, Spec};
use serde::Deserialize;
use serde_json::Value;

use crate::contract::{CONTRACT, Handle, KIND, Store};

/// A directory of checkpoint files.
#[derive(Debug, Clone)]
pub struct LocalStore {
    root: PathBuf,
}

impl LocalStore {
    /// Open (creating when missing) the store root.
    pub fn open(root: impl Into<PathBuf>) -> Result<Self> {
        let root = root.into();
        std::fs::create_dir_all(&root)?;
        Ok(Self {
            root: root.canonicalize()?,
        })
    }

    /// The canonical root.
    #[must_use]
    pub fn root(&self) -> &Path {
        &self.root
    }

    fn path(&self, exec_id: &str) -> Result<PathBuf> {
        let mut path = self.root.clone();
        for component in validate_exec_id(exec_id)? {
            path.push(component);
        }
        Ok(path)
    }
}

/// Exec ids are `/`-separated names, so a namespaced id like
/// `team-a/run-1` maps onto a nested directory. Every component is checked;
/// nothing can escape the store root.
fn validate_exec_id(exec_id: &str) -> Result<Vec<&str>> {
    if exec_id.is_empty() {
        return Err(Error::validation("exec_id is empty"));
    }
    if exec_id.len() > 512 {
        return Err(Error::validation("exec_id is longer than 512 bytes"));
    }
    let mut components = Vec::new();
    for component in exec_id.split('/') {
        if component.is_empty() || component == "." || component == ".." {
            return Err(Error::validation(format!(
                "exec_id {exec_id:?} has an unsafe path component {component:?}"
            )));
        }
        if !component
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '-' | '_' | '.'))
        {
            return Err(Error::validation(format!(
                "exec_id {exec_id:?} may only contain letters, digits, '.', '-', '_' and '/'"
            )));
        }
        components.push(component);
    }
    Ok(components)
}

#[async_trait]
impl Store for LocalStore {
    async fn save(&self, exec_id: &str, checkpoint: &[u8]) -> Result<()> {
        let path = self.path(exec_id)?;
        let directory = path
            .parent()
            .map(Path::to_path_buf)
            .unwrap_or_else(|| self.root.clone());
        let bytes = checkpoint.to_vec();
        tokio::task::spawn_blocking(move || {
            std::fs::create_dir_all(&directory)?;
            let mut temp = tempfile::NamedTempFile::new_in(&directory)?;
            std::io::Write::write_all(&mut temp, &bytes)?;
            temp.as_file().sync_all()?;
            temp.persist(&path)
                .map_err(|error| Error::internal(format!("checkpoint rename: {}", error.error)))?;
            Ok::<(), Error>(())
        })
        .await
        .map_err(|error| Error::internal(format!("checkpoint write task: {error}")))??;
        Ok(())
    }

    async fn load(&self, exec_id: &str) -> Result<Option<Vec<u8>>> {
        let path = self.path(exec_id)?;
        match tokio::fs::read(path).await {
            Ok(bytes) => Ok(Some(bytes)),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(None),
            Err(error) => Err(error.into()),
        }
    }

    async fn list(&self) -> Result<Vec<String>> {
        let root = self.root.clone();
        let mut ids = tokio::task::spawn_blocking(move || list_files(&root, &root))
            .await
            .map_err(|error| Error::internal(format!("checkpoint list task: {error}")))??;
        ids.sort();
        Ok(ids)
    }

    async fn delete(&self, exec_id: &str) -> Result<()> {
        let path = self.path(exec_id)?;
        match tokio::fs::remove_file(path).await {
            Ok(()) => Ok(()),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
            Err(error) => Err(error.into()),
        }
    }
}

/// Depth-first walk, returning `/`-joined paths relative to `root`.
fn list_files(root: &Path, directory: &Path) -> Result<Vec<String>> {
    let mut ids = Vec::new();
    let entries = match std::fs::read_dir(directory) {
        Ok(entries) => entries,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(ids),
        Err(error) => return Err(error.into()),
    };
    for entry in entries {
        let entry = entry?;
        let name = entry.file_name();
        let name = name.to_string_lossy();
        // Skip the temporary files an in-flight atomic save leaves behind.
        if name.starts_with(".tmp") {
            continue;
        }
        let path = entry.path();
        if entry.file_type()?.is_dir() {
            ids.extend(list_files(root, &path)?);
            continue;
        }
        let relative = path
            .strip_prefix(root)
            .map_err(|_| Error::internal("checkpoint path escaped its root"))?;
        ids.push(
            relative
                .to_string_lossy()
                .replace(std::path::MAIN_SEPARATOR, "/"),
        );
    }
    Ok(ids)
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct Settings {
    /// A directory, or a `${secret:...}` reference resolved at build time.
    root: flowcraft_resource::Secret,
}

fn validate(value: &Value) -> Result<()> {
    let settings: Settings = flowcraft_resource::settings(value)?;
    let empty = settings.root.is_empty()
        || settings
            .root
            .as_literal()
            .is_some_and(|root| root.trim().is_empty());
    if empty {
        return Err(Error::validation("settings.root is required"));
    }
    Ok(())
}

/// The `checkpoint.Store/local` factory.
pub struct LocalFactory;

impl Factory for LocalFactory {
    type Output = Handle;

    fn spec(&self) -> Spec {
        let kind = Kind::new(KIND).unwrap_or_else(|error| panic!("{KIND} is invalid: {error}"));
        Spec {
            contract: Some(CONTRACT),
            imp: Some("local"),
            deps: Vec::new(),
            phase: Phase::Main,
            validate_settings: Some(validate),
            ..Spec::new(kind)
        }
    }

    fn build<'a>(
        &'a self,
        ctx: BuildCtx<'a>,
    ) -> BoxFuture<'a, Result<std::sync::Arc<Self::Output>>> {
        Box::pin(async move {
            let settings: Settings = ctx.settings()?;
            // A `${secret:...}` root stays a reference until here: the value is
            // looked up once, when the store opens, and never lands in the
            // settings tree.
            let resolved = settings.root.resolve(ctx.secrets).await?;
            let mut root = PathBuf::from(resolved);
            if root.is_relative()
                && let Some(base) = ctx.base_dir
            {
                root = base.join(root);
            }
            let store = LocalStore::open(root)?;
            Ok(std::sync::Arc::new(Handle::new(std::sync::Arc::new(store))))
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn store() -> (tempfile::TempDir, LocalStore) {
        let dir = tempfile::tempdir().unwrap();
        let store = LocalStore::open(dir.path().join("checkpoints")).unwrap();
        (dir, store)
    }

    #[tokio::test]
    async fn save_load_list_delete_round_trip() {
        let (_dir, store) = store();
        store.save("run-1", b"one").await.unwrap();
        store.save("run-2", b"two").await.unwrap();
        assert_eq!(store.load("run-1").await.unwrap(), Some(b"one".to_vec()));
        assert_eq!(store.list().await.unwrap(), vec!["run-1", "run-2"]);
        store.delete("run-1").await.unwrap();
        store.delete("run-1").await.unwrap(); // idempotent
        assert_eq!(store.load("run-1").await.unwrap(), None);
        assert_eq!(store.list().await.unwrap(), vec!["run-2"]);
    }

    #[tokio::test]
    async fn saving_replaces_atomically() {
        let (_dir, store) = store();
        store.save("run-1", b"old").await.unwrap();
        store.save("run-1", b"new").await.unwrap();
        assert_eq!(store.load("run-1").await.unwrap(), Some(b"new".to_vec()));
    }

    #[tokio::test]
    async fn unsafe_ids_are_rejected() {
        let (_dir, store) = store();
        for id in ["../escape", "a//b", "a/./b", ".", "..", "", "a b"] {
            assert_eq!(
                store.load(id).await.unwrap_err().kind(),
                flowcraft_error::ErrorKind::Validation,
                "{id:?}"
            );
        }
    }

    #[tokio::test]
    async fn namespaced_ids_map_onto_directories() {
        let (_dir, store) = store();
        store.save("team-a/run-1", b"x").await.unwrap();
        assert_eq!(store.list().await.unwrap(), vec!["team-a/run-1"]);
        assert_eq!(
            store.load("team-a/run-1").await.unwrap(),
            Some(b"x".to_vec())
        );
    }

    #[tokio::test]
    async fn missing_checkpoint_is_none_not_an_error() {
        let (_dir, store) = store();
        assert_eq!(store.load("absent").await.unwrap(), None);
    }
}
