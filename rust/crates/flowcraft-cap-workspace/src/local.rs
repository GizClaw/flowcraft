//! The local provider: a directory tree with path confinement.

use std::path::{Component, Path, PathBuf};
use std::time::UNIX_EPOCH;

use async_trait::async_trait;
use flowcraft_error::{Error, ErrorKind, Result};
use flowcraft_resource::Kind;
use flowcraft_resource::{BoxFuture, BuildCtx, Factory, Spec};
use serde::Deserialize;
use serde_json::Value;

use crate::contract::{CONTRACT, Capabilities, Entry, Handle, KIND, Stat, Workspace};

/// A workspace rooted at one directory.
///
/// Paths are resolved component by component and `..` is rejected outright, so
/// a caller cannot leave the root through the path syntax. Symlinks are not
/// resolved here; moving to a capability-based handle (`cap-std`) is the
/// follow-up that removes that class entirely.
#[derive(Debug, Clone)]
pub struct LocalWorkspace {
    root: PathBuf,
}

impl LocalWorkspace {
    /// Open (creating when missing) a workspace root.
    pub fn open(root: impl Into<PathBuf>) -> Result<Self> {
        let root = root.into();
        std::fs::create_dir_all(&root)?;
        let root = root.canonicalize()?;
        Ok(Self { root })
    }

    /// The canonical root.
    #[must_use]
    pub fn root(&self) -> &Path {
        &self.root
    }

    fn resolve(&self, path: &str) -> Result<PathBuf> {
        if path.is_empty() {
            return Err(Error::validation("path is empty"));
        }
        if path.len() > 4096 {
            return Err(Error::validation("path is longer than 4096 bytes"));
        }
        let candidate = Path::new(path);
        if candidate.is_absolute() {
            return Err(Error::validation(format!(
                "path {path:?} must be relative to the workspace root"
            )));
        }
        let mut resolved = self.root.clone();
        for component in candidate.components() {
            match component {
                Component::Normal(part) => resolved.push(part),
                Component::CurDir => {}
                Component::ParentDir | Component::RootDir | Component::Prefix(_) => {
                    return Err(Error::validation(format!(
                        "path {path:?} escapes the workspace root"
                    )));
                }
            }
        }
        Ok(resolved)
    }

    async fn create_parents(&self, resolved: &Path) -> Result<()> {
        if let Some(parent) = resolved.parent() {
            tokio::fs::create_dir_all(parent).await?;
        }
        Ok(())
    }
}

#[async_trait]
impl Workspace for LocalWorkspace {
    async fn capabilities(&self) -> flowcraft_error::Result<Capabilities> {
        Ok(Capabilities {
            atomic_rename: true,
            read_after_write: true,
            durable_on_write: false,
            distributed: false,
        })
    }

    async fn read(&self, path: &str) -> Result<Vec<u8>> {
        let resolved = self.resolve(path)?;
        Ok(tokio::fs::read(resolved).await?)
    }

    async fn write(&self, path: &str, data: &[u8]) -> Result<()> {
        let resolved = self.resolve(path)?;
        self.create_parents(&resolved).await?;
        tokio::fs::write(resolved, data).await?;
        Ok(())
    }

    async fn append(&self, path: &str, data: &[u8]) -> Result<()> {
        use tokio::io::AsyncWriteExt;

        let resolved = self.resolve(path)?;
        self.create_parents(&resolved).await?;
        let mut file = tokio::fs::OpenOptions::new()
            .create(true)
            .append(true)
            .open(resolved)
            .await?;
        file.write_all(data).await?;
        file.flush().await?;
        Ok(())
    }

    async fn rename(&self, from: &str, to: &str) -> Result<()> {
        let from = self.resolve(from)?;
        let to = self.resolve(to)?;
        self.create_parents(&to).await?;
        tokio::fs::rename(from, to).await?;
        Ok(())
    }

    async fn delete(&self, path: &str) -> Result<()> {
        let resolved = self.resolve(path)?;
        match tokio::fs::remove_file(resolved).await {
            Ok(()) => Ok(()),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
            Err(error) => Err(error.into()),
        }
    }

    async fn remove_all(&self, path: &str) -> Result<()> {
        let resolved = self.resolve(path)?;
        if resolved == self.root {
            return Err(Error::forbidden("cannot remove the workspace root"));
        }
        match tokio::fs::remove_dir_all(resolved).await {
            Ok(()) => Ok(()),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
            Err(error) => Err(error.into()),
        }
    }

    async fn stat(&self, path: &str) -> Result<Stat> {
        let resolved = self.resolve(path)?;
        let metadata = tokio::fs::metadata(&resolved).await?;
        Ok(stat_of(path, &metadata))
    }

    async fn exists(&self, path: &str) -> Result<bool> {
        let resolved = self.resolve(path)?;
        match tokio::fs::metadata(resolved).await {
            Ok(_) => Ok(true),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(false),
            Err(error) => Err(error.into()),
        }
    }

    async fn list(&self, dir: &str) -> Result<Vec<Entry>> {
        let resolved = self.resolve(dir)?;
        let mut entries = Vec::new();
        let mut reader = match tokio::fs::read_dir(resolved).await {
            Ok(reader) => reader,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(entries),
            Err(error) => return Err(error.into()),
        };
        while let Some(entry) = reader.next_entry().await? {
            let metadata = entry.metadata().await?;
            let name = entry.file_name().to_string_lossy().into_owned();
            entries.push(entry_of(name, &metadata));
        }
        entries.sort_by(|left, right| left.name.cmp(&right.name));
        Ok(entries)
    }
}

fn stat_of(path: &str, metadata: &std::fs::Metadata) -> Stat {
    Stat {
        name: Path::new(path)
            .file_name()
            .map(|name| name.to_string_lossy().into_owned())
            .unwrap_or_else(|| path.to_owned()),
        size: metadata.len(),
        mode: mode_of(metadata),
        modified_unix_nanos: modified_nanos(metadata),
        is_dir: metadata.is_dir(),
    }
}

fn entry_of(name: String, metadata: &std::fs::Metadata) -> Entry {
    Entry {
        name,
        is_dir: metadata.is_dir(),
        mode: mode_of(metadata),
        size: metadata.len(),
        modified_unix_nanos: modified_nanos(metadata),
    }
}

#[cfg(unix)]
fn mode_of(metadata: &std::fs::Metadata) -> u32 {
    use std::os::unix::fs::PermissionsExt;
    metadata.permissions().mode()
}

#[cfg(not(unix))]
fn mode_of(_metadata: &std::fs::Metadata) -> u32 {
    0
}

fn modified_nanos(metadata: &std::fs::Metadata) -> i64 {
    metadata
        .modified()
        .ok()
        .and_then(|time| time.duration_since(UNIX_EPOCH).ok())
        .map_or(0, |duration| duration.as_nanos() as i64)
}

/// Settings for the local provider.
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

/// The `workspace.Workspace/local` factory.
pub struct LocalFactory;

impl Factory for LocalFactory {
    type Output = Handle;

    fn spec(&self) -> Spec {
        let kind = Kind::new(KIND).unwrap_or_else(|error| panic!("{KIND} is invalid: {error}"));
        Spec {
            contract: Some(CONTRACT),
            imp: Some("local"),
            deps: Vec::new(),
            phase: flowcraft_resource::Phase::Main,
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
            let workspace = LocalWorkspace::open(root)?;
            Ok(std::sync::Arc::new(Handle::new(std::sync::Arc::new(
                workspace,
            ))))
        })
    }
}

/// Whether a workspace error is a classification the caller should see.
#[allow(dead_code)]
fn is_not_found(error: &Error) -> bool {
    error.kind() == ErrorKind::NotFound
}

#[cfg(test)]
mod tests {
    use super::*;

    async fn workspace() -> (tempfile::TempDir, LocalWorkspace) {
        let dir = tempfile::tempdir().unwrap();
        let workspace = LocalWorkspace::open(dir.path().join("root")).unwrap();
        (dir, workspace)
    }

    #[tokio::test]
    async fn write_read_list_and_delete() {
        let (_dir, workspace) = workspace().await;
        workspace.write("a/b.txt", b"hello").await.unwrap();
        workspace.append("a/b.txt", b" world").await.unwrap();
        assert_eq!(workspace.read("a/b.txt").await.unwrap(), b"hello world");
        assert!(workspace.exists("a/b.txt").await.unwrap());
        let entries = workspace.list("a").await.unwrap();
        assert_eq!(entries.len(), 1);
        assert_eq!(entries[0].name, "b.txt");
        assert!(!entries[0].is_dir);
        workspace.delete("a/b.txt").await.unwrap();
        workspace.delete("a/b.txt").await.unwrap(); // idempotent
        assert!(!workspace.exists("a/b.txt").await.unwrap());
        assert!(workspace.list("missing").await.unwrap().is_empty());
    }

    #[tokio::test]
    async fn rename_publishes_atomically() {
        let (_dir, workspace) = workspace().await;
        workspace.write("tmp", b"payload").await.unwrap();
        workspace.rename("tmp", "live").await.unwrap();
        assert!(!workspace.exists("tmp").await.unwrap());
        assert_eq!(workspace.read("live").await.unwrap(), b"payload");
    }

    #[tokio::test]
    async fn traversal_is_rejected() {
        let (_dir, workspace) = workspace().await;
        for path in ["../escape", "a/../../escape", "/etc/passwd", "a/.."] {
            let error = workspace.read(path).await.unwrap_err();
            assert_eq!(error.kind(), ErrorKind::Validation, "{path}");
        }
    }

    #[tokio::test]
    async fn removing_the_root_is_forbidden() {
        let (_dir, workspace) = workspace().await;
        assert_eq!(
            workspace.remove_all(".").await.unwrap_err().kind(),
            ErrorKind::Forbidden
        );
    }

    #[tokio::test]
    async fn stat_reports_directories_and_files() {
        let (_dir, workspace) = workspace().await;
        workspace.write("dir/file", b"1234").await.unwrap();
        let file = workspace.stat("dir/file").await.unwrap();
        assert_eq!(file.size, 4);
        assert!(!file.is_dir);
        let dir = workspace.stat("dir").await.unwrap();
        assert!(dir.is_dir);
    }

    #[tokio::test]
    async fn capabilities_are_honest() {
        let (_dir, workspace) = workspace().await;
        let capabilities = workspace.capabilities().await.unwrap();
        assert!(capabilities.atomic_rename);
        assert!(!capabilities.distributed);
    }
}
