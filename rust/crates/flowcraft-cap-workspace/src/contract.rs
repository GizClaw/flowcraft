//! The workspace contract.

use std::sync::Arc;

use async_trait::async_trait;
use flowcraft_error::Result;
use flowcraft_resource::Resource;

/// The registry kind.
pub const KIND: &str = "workspace.Workspace";
/// The cross-process contract identity.
pub const CONTRACT: &str = "workspace.Workspace/v1";

/// What a provider can honestly guarantee.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct Capabilities {
    /// `rename` is atomic on the underlying medium.
    pub atomic_rename: bool,
    /// A read after a write observes the write.
    pub read_after_write: bool,
    /// A completed write survives a crash.
    pub durable_on_write: bool,
    /// The backend spans more than one node.
    pub distributed: bool,
}

/// One path's metadata.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Stat {
    /// Base name.
    pub name: String,
    /// Size in bytes.
    pub size: u64,
    /// Unix permission bits.
    pub mode: u32,
    /// Modification time, nanoseconds since the epoch.
    pub modified_unix_nanos: i64,
    /// Whether it is a directory.
    pub is_dir: bool,
}

/// One directory entry.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Entry {
    /// Base name.
    pub name: String,
    /// Whether it is a directory.
    pub is_dir: bool,
    /// Unix permission bits.
    pub mode: u32,
    /// Size in bytes.
    pub size: u64,
    /// Modification time, nanoseconds since the epoch.
    pub modified_unix_nanos: i64,
}

/// A filesystem-shaped workspace.
///
/// Paths are relative to the workspace root. Absolute paths and `..` are
/// rejected; `rename` is the "publish a finalized payload" operation; `delete`
/// and `remove_all` are idempotent.
#[async_trait]
pub trait Workspace: Send + Sync {
    /// What this provider guarantees.
    async fn capabilities(&self) -> Result<Capabilities>;

    /// Read a file.
    async fn read(&self, path: &str) -> Result<Vec<u8>>;

    /// Write a file, creating parent directories.
    async fn write(&self, path: &str, data: &[u8]) -> Result<()>;

    /// Append to a file, creating it and its parents when needed.
    async fn append(&self, path: &str, data: &[u8]) -> Result<()>;

    /// Move a file. The publication operation.
    async fn rename(&self, from: &str, to: &str) -> Result<()>;

    /// Remove a file. Idempotent.
    async fn delete(&self, path: &str) -> Result<()>;

    /// Remove a directory tree. Idempotent; the root is off limits.
    async fn remove_all(&self, path: &str) -> Result<()>;

    /// Metadata for one path.
    async fn stat(&self, path: &str) -> Result<Stat>;

    /// Whether a path exists. Missing is `false`, not an error.
    async fn exists(&self, path: &str) -> Result<bool>;

    /// Direct children, sorted by name. A missing directory is empty, not an
    /// error.
    async fn list(&self, dir: &str) -> Result<Vec<Entry>>;
}

/// The value a factory of this capability produces.
///
/// Requirements name this handle, not each provider type, so one endpoint can
/// serve a local provider or a future remote one without knowing which.
#[derive(Clone)]
pub struct Handle(pub Arc<dyn Workspace>);

impl Handle {
    /// Wrap a provider.
    #[must_use]
    pub fn new(workspace: Arc<dyn Workspace>) -> Self {
        Self(workspace)
    }

    /// The provider.
    #[must_use]
    pub fn workspace(&self) -> Arc<dyn Workspace> {
        self.0.clone()
    }
}

impl std::fmt::Debug for Handle {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("workspace.Handle")
    }
}

impl std::ops::Deref for Handle {
    type Target = Arc<dyn Workspace>;

    fn deref(&self) -> &Self::Target {
        &self.0
    }
}

impl Resource for Handle {}
