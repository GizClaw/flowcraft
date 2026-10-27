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
//!   store is world-listable until the first `set_permissions`.
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

    #[cfg(unix)]
    fn mode_of(path: &Path) -> u32 {
        use std::os::unix::fs::PermissionsExt as _;
        std::fs::metadata(path)
            .map(|metadata| metadata.permissions().mode() & 0o777)
            .unwrap_or(0)
    }
}
