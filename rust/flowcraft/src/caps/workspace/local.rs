//! The local provider: a directory tree pinned by a capability handle.
//!
//! # Why a handle and not a path string
//!
//! Every operation runs against a [`cap_std::fs::Dir`] opened on the root. The
//! handle resolves each component with `openat` (and `openat2` with
//! `RESOLVE_BENEATH` where the kernel has it), so the root is a *capability*
//! rather than a prefix comparison: a symlink swapped in after a check cannot
//! widen what the handle reaches, and a symlink that leaves the root is refused
//! by the kernel. This is the Rust counterpart of Go's `os.OpenRoot` in
//! `core/workspace`, which exists for the same reason.
//!
//! Two consequences are visible in behaviour and worth stating up front:
//!
//! * an absolute symlink is refused even when it points back inside the root,
//!   because `openat` cannot verify a target it does not resolve;
//! * `remove_all` on a final symlink removes the link, never the target tree.
//!
//! # Path syntax
//!
//! Paths are relative to the root; `""` and `.` name the root of the view. `..`
//! is resolved lexically first, and an operation is refused when the result
//! would climb above the root — the rule Go's `cleanPath` uses, so `a/../b.txt`
//! names `b.txt` while `../b.txt` is refused. Absolute paths are refused too.
//! Both refusals are [`ErrorKind::Forbidden`](crate::error::ErrorKind::Forbidden),
//! matching the class the Go side attaches to `ErrPathTraversal`; a missing path
//! is [`ErrorKind::NotFound`](crate::error::ErrorKind::NotFound).
//!
//! # Blocking work
//!
//! cap-std is a blocking API and the contract is async, so every operation hops
//! to [`tokio::task::spawn_blocking`]. The handle is shared behind an `Arc`, so
//! a task owns a clone for the duration of the call and no lock is involved.
//!
//! # Lifecycle
//!
//! There is no `close`. The handle is reference counted, so the root descriptor
//! is released when the last holder drops — which is the same moment the Go
//! provider's explicit `Close` would be called, minus the chance of forgetting
//! it. A decorator ([`crate::caps::workspace::scoped`], [`crate::caps::workspace::sub`]) holding the [`Handle`]
//! therefore keeps the root open exactly as long as it can be reached.

use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::UNIX_EPOCH;

use crate::error::{Error, Result};
use crate::resource::Kind;
use crate::resource::{BoxFuture, BuildCtx, Factory, Spec};
use async_trait::async_trait;
use cap_std::ambient_authority;
use cap_std::fs::{Dir, Metadata, OpenOptions};
use serde::Deserialize;
use serde_json::Value;

use crate::caps::workspace::codes::{self, Code};
use crate::caps::workspace::contract::{
    CONTRACT, Capabilities, Entry, Handle, KIND, Stat, Workspace,
};
use crate::caps::workspace::path;

/// File mode for created files, matching the Go provider.
#[cfg(unix)]
const FILE_MODE: u32 = 0o600;
/// Directory mode for created directories, matching the Go provider.
#[cfg(unix)]
const DIR_MODE: u32 = 0o700;

/// A workspace rooted at one directory.
///
/// The root is canonicalized once, at open, and every operation is resolved
/// relative to the pinned handle, so the root cannot be swapped for a symlink
/// afterwards.
#[derive(Debug, Clone)]
pub struct LocalWorkspace {
    root: PathBuf,
    dir: Arc<Dir>,
}

impl LocalWorkspace {
    /// Open (creating when missing) a workspace root.
    ///
    /// # Errors
    ///
    /// Fails when the directory cannot be created, canonicalized, or opened.
    // Assembly: there is no executor to block while a capability is being built.
    #[allow(clippy::disallowed_methods)]
    pub fn open(root: impl Into<PathBuf>) -> Result<Self> {
        let root = root.into();
        create_root(&root)?;
        // Resolve the root once: the handle below is opened on the real path,
        // so a later swap of the root for a symlink changes nothing.
        let root = std::fs::canonicalize(&root)?;
        let dir = Dir::open_ambient_dir(&root, ambient_authority())?;
        Ok(Self {
            root,
            dir: Arc::new(dir),
        })
    }

    /// The canonical root.
    #[must_use]
    pub fn root(&self) -> &Path {
        &self.root
    }

    /// Resolve a caller path to a path relative to the pinned handle.
    ///
    /// The lexical rules live in [`crate::caps::workspace::path`] so a view and a provider agree
    /// on them; the security boundary is the handle itself, which re-checks
    /// every component at use time.
    fn resolve(&self, path: &str) -> Result<PathBuf> {
        let cleaned = path::clean(path)?;
        Ok(PathBuf::from(if cleaned.is_empty() {
            ".".to_owned()
        } else {
            cleaned
        }))
    }

    /// Run one blocking operation off the async executor.
    async fn blocking<T, F>(&self, label: String, operation: F) -> Result<T>
    where
        T: Send + 'static,
        F: FnOnce(&Dir) -> Result<T> + Send + 'static,
    {
        let dir = Arc::clone(&self.dir);
        tokio::task::spawn_blocking(move || operation(&dir))
            .await
            .map_err(|error| {
                Error::internal(format!("workspace: {label}: blocking task failed: {error}"))
            })?
            .map_err(|error| annotate(&label, error))
    }
}

/// Add operation and path context to an error without changing its class.
///
/// The class is the part that travels a wire and that callers branch on, so it
/// survives; the message gains the operation that failed, with the module's own
/// `workspace: ` prefix stripped first so a nested failure reads once.
fn annotate(label: &str, error: Error) -> Error {
    let message = error.message();
    let message = message.strip_prefix("workspace: ").unwrap_or(message);
    Error::new(error.kind(), format!("workspace: {label}: {message}")).with_source(error)
}

/// Create the workspace root itself with the provider's mode.
///
/// `create_dir_all` would leave the root at `0o777 & !umask` (0755 on a normal
/// host) while every directory below it is 0700, so the root — the one a
/// deployment names in a document and shares between runs — would be the
/// readable one. The Go provider passes 0700 to `os.MkdirAll` for the same
/// reason.
// Assembly: the workspace root is created while the capability is being built.
#[allow(clippy::disallowed_methods)]
fn create_root(root: &Path) -> Result<()> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::DirBuilderExt;
        let mut builder = std::fs::DirBuilder::new();
        builder.recursive(true).mode(DIR_MODE);
        builder.create(root)?;
    }
    #[cfg(not(unix))]
    {
        std::fs::create_dir_all(root)?;
    }
    Ok(())
}

/// Create the missing parent directories of `path` with the provider's mode.
fn create_parents(dir: &Dir, path: &Path) -> Result<()> {
    let Some(parent) = path.parent() else {
        return Ok(());
    };
    if parent.as_os_str().is_empty() || parent == Path::new(".") {
        return Ok(());
    }
    #[cfg(unix)]
    {
        use cap_std::fs::{DirBuilder, DirBuilderExt};
        let mut builder = DirBuilder::new();
        builder.recursive(true).mode(DIR_MODE);
        dir.create_dir_with(parent, &builder)?;
    }
    #[cfg(not(unix))]
    {
        dir.create_dir_all(parent)?;
    }
    Ok(())
}

/// Whether this platform's `rename` can replace an existing directory.
///
/// `MoveFileEx`'s `REPLACE_EXISTING` cannot; POSIX `rename(2)` replaces an empty
/// one. This is the only platform-specific fact in the rename path — the
/// decision that uses it, [`check_rename`], is written once and tested
/// everywhere.
const RENAME_REPLACES_DIRECTORIES: bool = !cfg!(windows);

/// Refuse a rename the platform cannot express.
///
/// Directory rename is outside the contract, so replacing an existing directory
/// where the platform cannot is reported in the contract's own vocabulary
/// (a validation error) instead of leaking an OS code at the first call. Moving a
/// directory to a *new* name works everywhere and is left alone, and a
/// destination that does not exist cannot be a directory.
///
/// `replaces_directories` is [`RENAME_REPLACES_DIRECTORIES`]; taking it as a
/// parameter rather than consulting `cfg!` keeps the decision testable on any
/// host, which is the only way this branch gets exercised before a Windows
/// runner does.
fn check_rename(
    dir: &Dir,
    source: &Path,
    destination: &Path,
    replaces_directories: bool,
) -> Result<()> {
    if replaces_directories {
        return Ok(());
    }
    // `metadata` follows symlinks, matching the Go provider's `Stat` probes. A
    // missing path on either side means the rename cannot be replacing an
    // existing directory, so it may proceed; any other stat failure is reported
    // rather than swallowed, so a permission problem is not mistaken for
    // "nothing there".
    let metadata = |path: &Path| match dir.metadata(path) {
        Ok(metadata) => Ok(Some(metadata)),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(None),
        Err(error) => Err(Error::from(error)),
    };
    let (Some(source_meta), Some(destination_meta)) = (metadata(source)?, metadata(destination)?)
    else {
        return Ok(());
    };
    if source_meta.is_dir() || destination_meta.is_dir() {
        return Err(Error::validation(format!(
            "workspace: rename {} -> {}: replacing a directory is not supported",
            source.display(),
            destination.display()
        )));
    }
    Ok(())
}

/// Options for a fresh write: truncate, creating the file with the provider's
/// mode.
fn write_options() -> OpenOptions {
    let mut options = OpenOptions::new();
    options.write(true).create(true).truncate(true);
    #[cfg(unix)]
    {
        use cap_std::fs::OpenOptionsExt;
        options.mode(FILE_MODE);
    }
    options
}

/// Options for an append: create when missing, never truncate.
fn append_options() -> OpenOptions {
    let mut options = OpenOptions::new();
    options.append(true).create(true);
    #[cfg(unix)]
    {
        use cap_std::fs::OpenOptionsExt;
        options.mode(FILE_MODE);
    }
    options
}

fn mode_of(metadata: &Metadata) -> u32 {
    #[cfg(unix)]
    {
        use cap_std::fs::MetadataExt;
        metadata.mode()
    }
    #[cfg(not(unix))]
    {
        let _ = metadata;
        0
    }
}

fn modified_nanos(metadata: &Metadata) -> i64 {
    metadata
        .modified()
        .ok()
        .map(cap_std::time::SystemTime::into_std)
        .and_then(|time| time.duration_since(UNIX_EPOCH).ok())
        .map_or(0, |duration| duration.as_nanos() as i64)
}

/// The base name Go's `fs.FileInfo.Name` would report for `path`.
fn name_of(path: &Path) -> String {
    path.file_name()
        .map(|name| name.to_string_lossy().into_owned())
        .unwrap_or_else(|| path.display().to_string())
}

fn stat_of(path: &Path, metadata: &Metadata) -> Stat {
    Stat {
        name: name_of(path),
        size: metadata.len(),
        mode: mode_of(metadata),
        modified_unix_nanos: modified_nanos(metadata),
        is_dir: metadata.is_dir(),
    }
}

fn entry_of(name: String, metadata: &Metadata) -> Entry {
    Entry {
        name,
        is_dir: metadata.is_dir(),
        mode: mode_of(metadata),
        size: metadata.len(),
        modified_unix_nanos: modified_nanos(metadata),
    }
}

#[async_trait]
impl Workspace for LocalWorkspace {
    async fn capabilities(&self) -> Result<Capabilities> {
        Ok(Capabilities {
            atomic_rename: true,
            read_after_write: true,
            // Writes reach the OS page cache, not stable storage: the same
            // honest answer the Go provider gives.
            durable_on_write: false,
            distributed: false,
        })
    }

    async fn read(&self, path: &str) -> Result<Vec<u8>> {
        let target = self.resolve(path)?;
        self.blocking(format!("read {path}"), move |dir| Ok(dir.read(&target)?))
            .await
    }

    async fn read_limited(&self, path: &str, max_bytes: u64) -> Result<Vec<u8>> {
        if max_bytes == 0 {
            return Err(Error::validation(
                "workspace: read_limited max_bytes must be positive",
            ));
        }
        let target = self.resolve(path)?;
        let label = format!("read {path}");
        let shown = path.to_owned();
        self.blocking(label, move |dir| {
            let file = dir.open(&target)?;
            // Read at most one byte past the cap: enough to detect an oversized
            // file without materializing it.
            // `saturating_add`, not `+`: `max_bytes` is a `uint64` on the wire,
            // so `u64::MAX` is a legal request. Overflowing would panic in a
            // debug build and silently wrap to `take(0)` in release, answering
            // an empty payload for a non-empty file.
            let mut reader = std::io::Read::take(file, max_bytes.saturating_add(1));
            let mut data = Vec::new();
            std::io::Read::read_to_end(&mut reader, &mut data)?;
            if data.len() as u64 > max_bytes {
                return Err(codes::validation(
                    Code::TooLarge,
                    format!("workspace: {shown} exceeds the {max_bytes}-byte read cap"),
                ));
            }
            Ok(data)
        })
        .await
    }

    async fn write(&self, path: &str, data: &[u8]) -> Result<()> {
        let target = self.resolve(path)?;
        let bytes = data.to_vec();
        self.blocking(format!("write {path}"), move |dir| {
            create_parents(dir, &target)?;
            let mut file = dir.open_with(&target, &write_options())?;
            std::io::Write::write_all(&mut file, &bytes)?;
            Ok(())
        })
        .await
    }

    async fn append(&self, path: &str, data: &[u8]) -> Result<()> {
        let target = self.resolve(path)?;
        let bytes = data.to_vec();
        self.blocking(format!("append {path}"), move |dir| {
            create_parents(dir, &target)?;
            let mut file = dir.open_with(&target, &append_options())?;
            std::io::Write::write_all(&mut file, &bytes)?;
            std::io::Write::flush(&mut file)?;
            Ok(())
        })
        .await
    }

    async fn rename(&self, from: &str, to: &str) -> Result<()> {
        let source = self.resolve(from)?;
        let destination = self.resolve(to)?;
        self.blocking(format!("rename {from} -> {to}"), move |dir| {
            create_parents(dir, &destination)?;
            check_rename(dir, &source, &destination, RENAME_REPLACES_DIRECTORIES)?;
            dir.rename(&source, dir, &destination)?;
            Ok(())
        })
        .await
    }

    async fn delete(&self, path: &str) -> Result<()> {
        let target = self.resolve(path)?;
        // The error message names the caller's path, so it travels into the
        // blocking task.
        let label = format!("delete {path}");
        let shown = path.to_owned();
        self.blocking(label, move |dir| {
            // `symlink_metadata` is the right probe: a symlink is removed as a
            // link, never followed. Following it here would refuse a link to a
            // directory the contract says to unlink, and would treat a dangling
            // link as a missing path and leave it behind (the Go provider's
            // `Remove` unlinks it).
            match dir.symlink_metadata(&target) {
                Ok(metadata) if metadata.is_dir() => {
                    return Err(Error::validation(format!(
                        "workspace: {shown} is a directory (use remove_all)"
                    )));
                }
                Ok(_) => {}
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
                Err(error) => return Err(error.into()),
            }
            match crate::caps::workspace::retry::remove(|| dir.remove_file(&target)) {
                Ok(()) => Ok(()),
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
                Err(error) => Err(error.into()),
            }
        })
        .await
    }

    async fn remove_all(&self, path: &str) -> Result<()> {
        let target = self.resolve(path)?;
        if target == Path::new(".") {
            return Err(codes::forbidden(
                Code::RootRemoval,
                "cannot remove the workspace root",
            ));
        }
        self.blocking(format!("remove_all {path}"), move |dir| {
            // The contract names a tree, but removal is idempotent and total: a
            // file or a symlink at that path is removed too (Go's
            // `os.RemoveAll` does). Dispatching on the entry type keeps a link
            // from being followed and a file from failing with `ENOTDIR`.
            let directory = match dir.symlink_metadata(&target) {
                Ok(metadata) => metadata.is_dir(),
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
                Err(error) => return Err(error.into()),
            };
            let removed = if directory {
                crate::caps::workspace::retry::remove(|| dir.remove_dir_all(&target))
            } else {
                crate::caps::workspace::retry::remove(|| dir.remove_file(&target))
            };
            match removed {
                Ok(()) => Ok(()),
                // A concurrent remover got there first: removal is idempotent.
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
                Err(error) => Err(error.into()),
            }
        })
        .await
    }

    async fn stat(&self, path: &str) -> Result<Stat> {
        let target = self.resolve(path)?;
        self.blocking(format!("stat {path}"), move |dir| {
            let metadata = dir.metadata(&target)?;
            Ok(stat_of(&target, &metadata))
        })
        .await
    }

    async fn exists(&self, path: &str) -> Result<bool> {
        let target = self.resolve(path)?;
        self.blocking(format!("exists {path}"), move |dir| {
            match dir.try_exists(&target) {
                Ok(exists) => Ok(exists),
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(false),
                Err(error) => Err(error.into()),
            }
        })
        .await
    }

    async fn list(&self, dir: &str) -> Result<Vec<Entry>> {
        let target = self.resolve(dir)?;
        self.blocking(format!("list {dir}"), move |handle| {
            let mut reader = match handle.read_dir(&target) {
                Ok(reader) => reader,
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
                Err(error) => return Err(error.into()),
            };
            let mut entries = Vec::new();
            for entry in &mut reader {
                let entry = entry?;
                let name = entry.file_name().to_string_lossy().into_owned();
                let metadata = entry.metadata()?;
                entries.push(entry_of(name, &metadata));
            }
            entries.sort_by(|left, right| left.name.cmp(&right.name));
            Ok(entries)
        })
        .await
    }
}

/// Settings for the local provider.
#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct Settings {
    /// A directory, or a `${secret:...}` reference resolved at build time.
    root: crate::resource::Secret,
}

fn validate(value: &Value) -> Result<()> {
    let settings: Settings = crate::resource::settings(value)?;
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
            phase: crate::resource::Phase::Main,
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

#[cfg(test)]
mod tests {
    use super::*;
    use crate::error::ErrorKind;

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

    /// The Windows decision, exercised on every host by passing the flag the
    /// platform would pass.
    #[test]
    fn replacing_a_directory_is_refused_only_where_the_platform_cannot() {
        let dir = tempfile::tempdir().unwrap();
        let handle = Dir::open_ambient_dir(dir.path(), ambient_authority()).unwrap();
        handle.create_dir_all("tree").unwrap();
        handle.write("file", b"x").unwrap();
        handle.write("other", b"y").unwrap();

        // POSIX rename(2) replaces a file or an empty directory.
        for (source, destination) in [("tree", "file"), ("file", "tree")] {
            assert!(
                check_rename(&handle, Path::new(source), Path::new(destination), true).is_ok(),
                "{source} -> {destination}"
            );
        }

        // Windows cannot, in either direction, once both paths exist.
        for (source, destination) in [("tree", "file"), ("file", "tree")] {
            let error = check_rename(&handle, Path::new(source), Path::new(destination), false)
                .unwrap_err();
            assert_eq!(
                error.kind(),
                ErrorKind::Validation,
                "{source} -> {destination}"
            );
            assert!(error.to_string().contains("replacing a directory"));
        }

        // Two files are a plain replacement, and a destination that does not
        // exist cannot be a directory, so moving a directory to a new name works.
        assert!(check_rename(&handle, Path::new("file"), Path::new("other"), false).is_ok());
        assert!(check_rename(&handle, Path::new("tree"), Path::new("fresh"), false).is_ok());
    }

    /// Only runs on Windows: this is the end-to-end shape of the branch above.
    #[cfg(windows)]
    #[tokio::test]
    async fn rename_over_a_directory_is_a_validation_error() {
        let (_dir, workspace) = workspace().await;
        workspace.write("tree/file", b"x").await.unwrap();
        workspace.write("other", b"y").await.unwrap();
        let error = workspace.rename("tree", "other").await.unwrap_err();
        assert_eq!(error.kind(), ErrorKind::Validation);
        // A directory moved to a new name is still fine.
        workspace.rename("tree", "moved").await.unwrap();
        assert_eq!(workspace.read("moved/file").await.unwrap(), b"x");
    }

    /// Only runs on Windows, where there are no unix mode bits to report.
    #[cfg(windows)]
    #[tokio::test]
    async fn stat_reports_no_mode_bits_without_unix_permissions() {
        let (_dir, workspace) = workspace().await;
        workspace.write("file", b"x").await.unwrap();
        assert_eq!(workspace.stat("file").await.unwrap().mode, 0);
    }

    #[tokio::test]
    async fn traversal_is_forbidden() {
        let (_dir, workspace) = workspace().await;
        for path in ["..", "../escape", "a/../../escape", "/etc/passwd"] {
            let error = workspace.read(path).await.unwrap_err();
            assert_eq!(error.kind(), ErrorKind::Forbidden, "{path}");
        }
    }

    #[tokio::test]
    async fn an_interior_parent_dir_is_cleaned_not_refused() {
        let (_dir, workspace) = workspace().await;
        workspace.write("b.txt", b"payload").await.unwrap();
        assert_eq!(workspace.read("a/../b.txt").await.unwrap(), b"payload");
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn a_symlink_cannot_escape_the_root() {
        let (dir, workspace) = workspace().await;
        let outside = dir.path().join("outside.txt");
        std::fs::write(&outside, b"secret").unwrap();
        std::os::unix::fs::symlink(&outside, workspace.root().join("escape")).unwrap();
        let error = workspace.read("escape").await.unwrap_err();
        assert_eq!(error.kind(), ErrorKind::Forbidden);
        // An absolute link is refused even when it points back inside the root:
        // `openat` cannot verify a target it does not resolve itself.
        std::os::unix::fs::symlink(
            workspace.root().join("nested"),
            workspace.root().join("absolute"),
        )
        .unwrap();
        workspace.write("nested/file", b"inside").await.unwrap();
        assert_eq!(
            workspace.read("absolute/file").await.unwrap_err().kind(),
            ErrorKind::Forbidden
        );
        // A relative link inside the root still works.
        std::os::unix::fs::symlink("nested/file", workspace.root().join("inside")).unwrap();
        assert_eq!(workspace.read("inside").await.unwrap(), b"inside");
    }

    /// Removal is total: a file at the path is removed, not reported as
    /// "not a directory" (Go's `os.RemoveAll` behaves the same).
    #[tokio::test]
    async fn remove_all_also_removes_a_file() {
        let (_dir, workspace) = workspace().await;
        workspace.write("file.txt", b"x").await.unwrap();
        workspace.remove_all("file.txt").await.unwrap();
        assert!(!workspace.exists("file.txt").await.unwrap());
        // Still idempotent.
        workspace.remove_all("file.txt").await.unwrap();
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn delete_unlinks_a_symlink_without_following_it() {
        let (dir, workspace) = workspace().await;
        // A link to a directory is unlinked, not refused: the contract removes
        // files, and a link is an entry of its own.
        std::fs::create_dir_all(workspace.root().join("tree")).unwrap();
        std::os::unix::fs::symlink(
            workspace.root().join("tree"),
            workspace.root().join("link-to-dir"),
        )
        .unwrap();
        workspace.delete("link-to-dir").await.unwrap();
        assert!(!workspace.exists("link-to-dir").await.unwrap());
        assert!(workspace.exists("tree").await.unwrap());

        // A dangling link is an entry too, and used to be left behind because
        // the follow-based probe answered NotFound.
        std::os::unix::fs::symlink(dir.path().join("gone"), workspace.root().join("dangling"))
            .unwrap();
        workspace.delete("dangling").await.unwrap();
        assert!(
            std::fs::symlink_metadata(workspace.root().join("dangling")).is_err(),
            "the dangling link should be gone"
        );
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn the_root_itself_is_private() {
        use std::os::unix::fs::PermissionsExt;
        let (_dir, workspace) = workspace().await;
        let mode = std::fs::metadata(workspace.root())
            .unwrap()
            .permissions()
            .mode()
            & 0o777;
        assert_eq!(mode, 0o700);
    }

    #[tokio::test]
    async fn removing_the_root_is_forbidden() {
        let (_dir, workspace) = workspace().await;
        for path in [".", ""] {
            assert_eq!(
                workspace.remove_all(path).await.unwrap_err().kind(),
                ErrorKind::Forbidden
            );
        }
    }

    #[tokio::test]
    async fn deleting_a_directory_is_a_validation_error() {
        let (_dir, workspace) = workspace().await;
        workspace.write("dir/file", b"x").await.unwrap();
        let error = workspace.delete("dir").await.unwrap_err();
        assert_eq!(error.kind(), ErrorKind::Validation);
        assert!(error.to_string().contains("remove_all"));
        workspace.remove_all("dir").await.unwrap();
        assert!(!workspace.exists("dir").await.unwrap());
    }

    #[tokio::test]
    async fn stat_reports_directories_and_files() {
        let (_dir, workspace) = workspace().await;
        workspace.write("dir/file", b"1234").await.unwrap();
        let file = workspace.stat("dir/file").await.unwrap();
        assert_eq!(file.size, 4);
        assert!(!file.is_dir);
        assert_eq!(file.name, "file");
        let dir = workspace.stat("dir").await.unwrap();
        assert!(dir.is_dir);
        assert_eq!(dir.name, "dir");
        assert_eq!(workspace.stat(".").await.unwrap().name, ".");
    }

    #[tokio::test]
    async fn a_missing_path_is_not_found() {
        let (_dir, workspace) = workspace().await;
        assert_eq!(
            workspace.read("missing").await.unwrap_err().kind(),
            ErrorKind::NotFound
        );
        assert_eq!(
            workspace.stat("missing").await.unwrap_err().kind(),
            ErrorKind::NotFound
        );
        assert!(!workspace.exists("missing").await.unwrap());
    }

    #[tokio::test]
    async fn bounded_reads_stop_at_the_cap() {
        let (_dir, workspace) = workspace().await;
        workspace.write("file", b"0123456789").await.unwrap();
        assert_eq!(workspace.read_limited("file", 10).await.unwrap().len(), 10);
        // `u64::MAX` is a legal wire value: the cap arithmetic must not
        // overflow into an empty answer.
        assert_eq!(
            workspace.read_limited("file", u64::MAX).await.unwrap(),
            b"0123456789"
        );
        let error = workspace.read_limited("file", 9).await.unwrap_err();
        assert_eq!(error.kind(), ErrorKind::Validation);
        assert_eq!(
            workspace.read_limited("file", 0).await.unwrap_err().kind(),
            ErrorKind::Validation
        );
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn created_files_and_directories_are_private() {
        use std::os::unix::fs::PermissionsExt;
        let (_dir, workspace) = workspace().await;
        workspace.write("dir/file", b"x").await.unwrap();
        let file = workspace.root().join("dir/file");
        let mode = std::fs::metadata(&file).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode, 0o600);
        let dir = std::fs::metadata(workspace.root().join("dir"))
            .unwrap()
            .permissions()
            .mode()
            & 0o777;
        assert_eq!(dir, 0o700);
    }

    #[tokio::test]
    async fn capabilities_are_honest() {
        let (_dir, workspace) = workspace().await;
        let capabilities = workspace.capabilities().await.unwrap();
        assert!(capabilities.atomic_rename);
        assert!(!capabilities.distributed);
    }
}
