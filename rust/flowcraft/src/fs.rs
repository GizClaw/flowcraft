//! The file modes that carry security meaning.
//!
//! A capability may use `std::fs` directly for everything else. These two may
//! not be open-coded, because each has already been got wrong in this crate once:
//!
//! - **a private file**: `File::create` then `set_permissions(0o600)` leaves a
//!   window — every rotation re-opens the log, so the window recurs — in which
//!   the file exists with the process's umask permissions. The mode must be part
//!   of the `open` call, and the name must be claimed atomically
//!   (`create_new`) so two writers cannot share one file.
//! - **a private directory**: the mode must be set at creation, or a nested
//!   store is world-listable until the first `set_permissions`;
//! - **reading out of one**: `check then read` is not a boundary. A path-based
//!   check (`symlink_metadata` → `canonicalize` → `read`) can be raced by
//!   swapping a parent directory for a symlink between the check and the use, and
//!   then the read lands wherever the attacker pointed it. [`PrivateDir`] holds
//!   an open handle on the directory instead, so every resolution is relative to
//!   that handle and cannot leave it.
//!
//! This module deliberately returns `io::Result` instead of the crate's `Error`:
//! the callers have their own vocabulary for a failed write (`secret keychain:
//! …`, `telemetry: …`), and wrapping here would force one wording on all of
//! them. It sits on nothing but `std`.

use std::fs::{File, OpenOptions};
use std::path::Path;

/// Owner read/write only.
pub const FILE_MODE: u32 = 0o600;
/// Owner only.
pub const DIR_MODE: u32 = 0o700;

/// Create `path`, exclusively, readable and writable only by its owner from the
/// moment it exists.
///
/// `create_new` is what makes the claim atomic: the caller may treat
/// `AlreadyExists` as "someone else owns this name", which is exactly what the
/// key-mint path needs.
pub fn private_file(path: &Path) -> std::io::Result<File> {
    open_private(path, false)
}

/// Open `path` for appending, creating it privately if it does not exist.
///
/// The log-file case: a rotation re-opens the same path, so the mode has to be
/// part of the open rather than a follow-up `chmod` — otherwise the window
/// recurs on every rotation, on a file that carries tenant names and paths.
pub fn private_append(path: &Path) -> std::io::Result<File> {
    open_private(path, true)
}

fn open_private(path: &Path, append: bool) -> std::io::Result<File> {
    let mut options = OpenOptions::new();
    options.write(true);
    if append {
        options.create(true).append(true);
    } else {
        options.create_new(true);
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt as _;
        options.mode(FILE_MODE);
    }
    #[cfg(not(unix))]
    {
        // No mode to set: the platform's own ACL inheritance is the mechanism,
        // and there is nothing this call can get wrong.
        if append {
            options.create(true).append(true);
        }
    }
    // `mode` only applies to a created file, which is exactly the case that
    // matters: an existing file keeps whatever it already had, so a caller that
    // needs 0600 guaranteed must create it (or tighten it explicitly).
    options.open(path)
}

/// Create `path` and its parents, owner-only.
///
/// Idempotent. An existing directory is **not** re-permissioned: silently
/// changing the mode of a directory a deployment chose would be surprising, and
/// a store that must be 0700 checks it instead (see the keychain).
pub fn create_private_dir(path: &Path) -> std::io::Result<()> {
    if path.as_os_str().is_empty() {
        return Ok(());
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::DirBuilderExt as _;
        // `create_dir_all` cannot set a mode; `DirBuilder::mode` applies to every
        // directory this call creates, which is what a store wants.
        let mut builder = std::fs::DirBuilder::new();
        builder.recursive(true).mode(DIR_MODE);
        builder.create(path)
    }
    #[cfg(not(unix))]
    {
        std::fs::create_dir_all(path)
    }
}

/// The mode `path` currently has, or `None` where the platform has no modes.
#[must_use]
pub fn private_mode_ok(path: &Path) -> Option<bool> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt as _;
        let metadata = std::fs::metadata(path).ok()?;
        Some(metadata.permissions().mode() & 0o777 == FILE_MODE)
    }
    #[cfg(not(unix))]
    {
        let _ = path;
        None
    }
}

/// A directory handle that cannot be talked out of its root.
///
/// The difference from `std::fs` is the *handle*: the root is opened once, and
/// every lookup is resolved from that open directory. A path check can be raced
/// — swap a parent for a symlink between `canonicalize` and `read`, and the read
/// follows it — while a resolution that starts at the handle has nothing to swap:
/// the worst a race can do is redirect to another file *inside* the directory,
/// which the caller could have asked for by name anyway.
///
/// Behind [`feature = "private-dir"`] because the mechanism (`cap-std`) is not
/// free and only the secret store needs it today; the two earlier functions in
/// this module are `std` and always compiled.
#[cfg(feature = "private-dir")]
#[derive(Clone)]
pub struct PrivateDir {
    root: std::sync::Arc<std::path::PathBuf>,
    dir: std::sync::Arc<cap_std::fs::Dir>,
}

/// Why a private read produced no value.
#[cfg(feature = "private-dir")]
#[derive(Debug)]
pub enum ReadFailure {
    /// The name is not in the directory.
    Missing,
    /// The entry is a symlink. A private directory refuses to follow one, so a
    /// `secret` cannot be smuggled in by linking to something else.
    Symlink,
    /// The entry exists but is not a regular file.
    NotAFile,
    /// The file is larger than the caller's bound.
    TooLarge(u64),
    /// The name resolves outside the directory.
    Escapes,
    /// Anything else the filesystem said.
    Io(std::io::Error),
}

#[cfg(feature = "private-dir")]
impl std::fmt::Debug for PrivateDir {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("fs.PrivateDir")
            .field("root", &self.root)
            .finish_non_exhaustive()
    }
}

#[cfg(feature = "private-dir")]
impl PrivateDir {
    /// Open `root` as the handle every read is resolved from.
    pub fn open(root: &std::path::Path) -> std::io::Result<Self> {
        Ok(Self {
            root: std::sync::Arc::new(root.to_path_buf()),
            dir: std::sync::Arc::new(cap_std::fs::Dir::open_ambient_dir(
                root,
                cap_std::ambient_authority(),
            )?),
        })
    }

    /// The root this handle was opened on.
    #[must_use]
    pub fn root(&self) -> &std::path::Path {
        self.root.as_path()
    }

    /// Read `name` out of the directory, refusing a symlink and bounding the size.
    ///
    /// The size bound is enforced while reading, not from a `stat` taken before
    /// it, so a file that grows between the two cannot get past it.
    pub fn read(&self, name: &str, max_bytes: u64) -> Result<Vec<u8>, ReadFailure> {
        use std::io::Read as _;

        let metadata = match self.dir.symlink_metadata(name) {
            Ok(metadata) => metadata,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                return Err(ReadFailure::Missing);
            }
            Err(error) => return Err(ReadFailure::Io(error)),
        };
        if metadata.is_symlink() {
            return Err(ReadFailure::Symlink);
        }
        if !metadata.is_file() {
            return Err(ReadFailure::NotAFile);
        }
        // From here on the resolution starts at the handle: no path can escape it,
        // whatever happens to the directories in between.
        let file = match self.dir.open(name) {
            Ok(file) => file,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                return Err(ReadFailure::Missing);
            }
            // `cap-std` reports "this would leave the directory" as a permission
            // error, which is the one case worth distinguishing by kind.
            Err(error) if error.kind() == std::io::ErrorKind::PermissionDenied => {
                return Err(ReadFailure::Escapes);
            }
            Err(error) => return Err(ReadFailure::Io(error)),
        };
        let mut data = Vec::new();
        file.take(max_bytes.saturating_add(1))
            .read_to_end(&mut data)
            .map_err(ReadFailure::Io)?;
        let length = data.len() as u64;
        if length > max_bytes {
            return Err(ReadFailure::TooLarge(length));
        }
        Ok(data)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write as _;

    #[test]
    fn a_created_file_is_private_from_its_first_byte() {
        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("private");
        let mut file = private_file(&path).expect("create");
        file.write_all(b"x").expect("write");
        drop(file);
        // The mode is not a follow-up `chmod`: it was set by `open`.
        if let Some(ok) = private_mode_ok(&path) {
            assert!(ok, "the file is {:o}", mode_of(&path));
        }
    }

    #[test]
    fn a_second_create_of_the_same_name_fails() {
        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("private");
        drop(private_file(&path).expect("create"));
        let error = private_file(&path).expect_err("already exists");
        assert_eq!(error.kind(), std::io::ErrorKind::AlreadyExists);
    }

    #[test]
    fn an_appended_file_is_private_and_keeps_its_bytes() {
        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("log");
        private_append(&path)
            .expect("create")
            .write_all(b"one\n")
            .expect("write");
        private_append(&path)
            .expect("append")
            .write_all(b"two\n")
            .expect("write");
        assert_eq!(std::fs::read(&path).expect("read"), b"one\ntwo\n");
        if let Some(ok) = private_mode_ok(&path) {
            assert!(ok, "the file is {:o}", mode_of(&path));
        }
    }

    /// The handle refuses what the path-based reader used to check by hand.
    #[cfg(all(unix, feature = "private-dir"))]
    #[test]
    fn a_private_read_refuses_a_symlink_and_bounds_the_size() {
        use crate::fs::ReadFailure;

        let home = tempfile::tempdir().expect("tempdir");
        let dir = PrivateDir::open(home.path()).expect("open");
        std::fs::write(home.path().join("plain"), b"value").expect("write");

        assert_eq!(dir.read("plain", 1024).expect("read"), b"value");
        assert!(matches!(
            dir.read("absent", 1024).expect_err("missing"),
            ReadFailure::Missing
        ));

        // A symlink, wherever it points, is not a secret: following one is how a
        // store gets read out of a directory it never opened.
        let outside = tempfile::tempdir().expect("tempdir");
        std::fs::write(outside.path().join("secret"), b"nope").expect("write");
        std::os::unix::fs::symlink(outside.path().join("secret"), home.path().join("link"))
            .expect("symlink");
        assert!(matches!(
            dir.read("link", 1024).expect_err("symlink"),
            ReadFailure::Symlink
        ));

        // The size bound is enforced while reading, not from the `stat` above it.
        assert!(matches!(
            dir.read("plain", 2).expect_err("too large"),
            ReadFailure::TooLarge(_)
        ));
    }

    #[cfg(all(unix, feature = "private-dir"))]
    #[test]
    fn a_private_read_cannot_leave_its_directory() {
        use crate::fs::ReadFailure;

        let home = tempfile::tempdir().expect("tempdir");
        let dir = PrivateDir::open(home.path()).expect("open");
        // `..` is not a spelling the handle honours, and a nested symlinked
        // directory is refused at the entry itself.
        assert!(matches!(
            dir.read("../escape", 1024).expect_err("traversal"),
            ReadFailure::Escapes | ReadFailure::Missing | ReadFailure::Io(_)
        ));
    }

    #[test]
    fn a_private_directory_is_created_with_its_parents() {
        let dir = tempfile::tempdir().expect("tempdir");
        let nested = dir.path().join("a").join("b");
        create_private_dir(&nested).expect("create");
        assert!(nested.is_dir());
        // Re-running is a no-op, not an error.
        create_private_dir(&nested).expect("idempotent");
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt as _;
            let mode = std::fs::metadata(&nested)
                .expect("metadata")
                .permissions()
                .mode();
            assert_eq!(mode & 0o777, DIR_MODE, "the directory is {mode:o}");
        }
    }

    /// The mode, for a failure message. Defined on every target so the assertions
    /// above compile where modes do not exist (they are guarded by
    /// [`private_mode_ok`] returning `None`).
    fn mode_of(path: &Path) -> u32 {
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt as _;
            std::fs::metadata(path)
                .map(|metadata| metadata.permissions().mode() & 0o777)
                .unwrap_or(0)
        }
        #[cfg(not(unix))]
        {
            let _ = path;
            0
        }
    }
}
