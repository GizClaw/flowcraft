//! The local backend: a directory tree on the host filesystem.
//!
//! `core/workspace/local.go` pins its root with an `*os.Root`: a directory
//! handle, so every operation resolves beneath *it* rather than beneath a string,
//! and a hostile writer who swaps a component for a symlink between the check and
//! the use has nothing to win. That containment is the whole reason this backend
//! is not "`std::fs` plus a `..` check", and it is why the port takes
//! [`cap_std::fs::Dir`]: the same primitive (a directory descriptor, per-component
//! resolution beneath it), with the same rules at the edges.
//!
//! The trait above stays path-shaped -- core's interface takes strings, and a
//! capability-style API would be a rewrite of every caller, not a port
//! (`docs/plans/2026-10-15-rust-core-port.md` §8 question 6). `Dir` is therefore
//! an implementation detail of this module.
//!
//! # What a pinned root decides, and the port inherits
//!
//! - An **absolute** path is refused before it reaches the handle ([`clean_path`]),
//!   and so is one that climbs with `..`. The refusal is lexical: `..a` too.
//! - A symlink **inside** the tree is followed only while it stays inside it, and
//!   the target must be **relative** -- an absolute link is refused even when it
//!   points back into the tree. That is `os.Root`'s rule, and it is a rule of the
//!   handle rather than of any check we could have skipped.
//! - `remove_all` on a final escaping symlink removes **the link**, never the
//!   target. `delete` is not `remove_all`: it stats first, so it *fails* on a link
//!   that escapes (core's `Delete` propagates the stat's error rather than
//!   treating it as "not there"). [`LocalWorkspace::delete`] keeps that, because a
//!   caller who means "remove the link" wants [`Workspace::remove_all`].
//! - A view created by [`LocalWorkspace::sub`] shares the handle and carries a
//!   relative prefix, so symlinks beneath the prefix still resolve only inside the
//!   *root*, not inside the prefix.
//!
//! # What is *not* portable, and is therefore not asserted
//!
//! Failures that come from the host (a permission bit, `EISDIR`, an escape the
//! handle detected) carry the platform's sentence in core (`openat a: no such file
//! or directory`) and carry cap-std's here. The conformance rows mark those `os`:
//! what they assert is the *outcome* and the classified [`ErrorKind`], not the
//! words. Sentences this module builds itself (traversal, not-found, the
//! directory-instead-of-file refusal, the read cap) are core's, and are asserted
//! exactly.
//!
//! Windows is its own slice (`docs/plans/2026-10-15-rust-core-port.md` §12): core's
//! `rename` guard against replacing a directory, and `MoveFileEx`'s behaviour, are
//! not here.

use std::io::{self, Read, Write};
use std::path::{Path, PathBuf};
use std::sync::Arc;

use cap_std::ambient_authority;
use cap_std::fs::{Dir, OpenOptions as CapOpenOptions};
use flowcraft_contract::error::{Error, ErrorKind};

use crate::clean::{base, clean_path, join};
use crate::refusal;
use crate::{Capabilities, Entry, LimitedReader, Meta, Result, Workspace};

/// A workspace backed by a directory tree on the host filesystem.
///
/// Constructing one creates the root if it is missing and resolves it through
/// symlinks, then opens the handle: the root itself cannot be swapped for a
/// symlink afterwards.
#[derive(Debug, Clone)]
pub struct LocalWorkspace {
    /// The display path of this view: the resolved root, plus the prefix.
    root: PathBuf,
    /// The pinned handle of the top level, shared by every view.
    ///
    /// Shared rather than duplicated: core's views borrow the top-level
    /// `*os.Root`, and only the top level owns the descriptor.
    dir: Arc<Dir>,
    /// This view's path inside `dir`, `/`-separated; empty at the top level.
    prefix: String,
}

impl LocalWorkspace {
    /// Opens (creating it if needed) the workspace rooted at `root`.
    ///
    /// # Errors
    ///
    /// A root that cannot be created, resolved or opened.
    pub fn new(root: impl AsRef<Path>) -> Result<Self> {
        let requested = root.as_ref();
        std::fs::create_dir_all(requested).map_err(|err| {
            Error::new(
                ErrorKind::Internal,
                format!("workspace: create root: {err}"),
            )
        })?;
        let resolved = std::fs::canonicalize(requested).map_err(|err| {
            Error::new(
                ErrorKind::Internal,
                format!("workspace: eval symlinks for root: {err}"),
            )
        })?;
        let dir = Dir::open_ambient_dir(&resolved, ambient_authority()).map_err(|err| {
            Error::new(
                ErrorKind::Internal,
                format!("workspace: open root {}: {err}", resolved.display()),
            )
        })?;
        Ok(LocalWorkspace {
            root: resolved,
            dir: Arc::new(dir),
            prefix: String::new(),
        })
    }

    /// The display path of this view.
    ///
    /// For a view from [`LocalWorkspace::sub`] it is the logical join of the
    /// parent's root and the prefix, and it may contain symlink components that the
    /// handle resolves internally. It is for display, or for a one-shot open while
    /// no hostile writer can swap components; the [`Workspace`] methods stay
    /// race-free without it.
    pub fn root(&self) -> &Path {
        &self.root
    }

    /// A view rooted under `prefix`, creating the directory if it is missing.
    ///
    /// The view shares the parent's handle and prefixes every operation.
    ///
    /// # Errors
    ///
    /// A prefix that escapes ([`crate::refusal::path_traversal`]), or a directory
    /// that cannot be created beneath the handle.
    pub fn sub(&self, prefix: &str) -> Result<Self> {
        let cleaned = clean_path(prefix).map_err(|err| {
            Error::new(
                err.kind(),
                format!("workspace local sub: invalid prefix {prefix:?}: {err}"),
            )
        })?;
        let Some(cleaned) = cleaned else {
            return Ok(self.clone());
        };
        let rel = join(&[&self.prefix, &cleaned]);
        let display = self.root.join(&cleaned);

        // Advisory: the handle below is the boundary. This only decides which
        // refusal a caller sees, so a link that leaves the tree is named as a
        // traversal rather than as an unexplained host error.
        if let Ok(real) = resolve_existing_prefix(&display)
            && !within(&real, &self.root)
        {
            return Err(Error::new(
                ErrorKind::Forbidden,
                format!("{}: {cleaned} (symlink escape)", refusal::TRAVERSAL),
            ));
        }
        self.dir.create_dir_all(&rel).map_err(|err| {
            Error::new(
                ErrorKind::Internal,
                format!("workspace local sub: create {cleaned:?}: {err}"),
            )
        })?;
        Ok(LocalWorkspace {
            root: display,
            dir: Arc::clone(&self.dir),
            prefix: rel,
        })
    }

    /// `path` as written by the caller, mapped inside the pinned handle.
    ///
    /// `""` and `"."` both mean the root of this view.
    fn rel(&self, path: &str) -> Result<String> {
        let cleaned = clean_path(path)?.unwrap_or_else(|| ".".to_owned());
        Ok(join(&[&self.prefix, &cleaned]))
    }

    /// The name [`Workspace::stat`] reports for `path`.
    ///
    /// core answers `fs.FileInfo`, whose `Name` is the final element of the path
    /// that was stat-ed -- the argument's, not the resolved file's.
    fn name_of(&self, path: &str) -> String {
        let named = base(path);
        if named.is_empty() || named == "." {
            return base(&self.root.to_string_lossy());
        }
        named.to_owned()
    }
}

impl Workspace for LocalWorkspace {
    fn read(&self, path: &str) -> Result<Vec<u8>> {
        let rel = self.rel(path)?;
        self.dir.read(&rel).map_err(|err| {
            if err.kind() == io::ErrorKind::NotFound {
                refusal::not_found(path)
            } else {
                host_error("read", path, err)
            }
        })
    }

    fn write(&self, path: &str, data: &[u8]) -> Result<()> {
        let rel = self.rel(path)?;
        self.dir
            .create_dir_all(parent(&rel))
            .map_err(|err| host_error(&format!("mkdir for {path}"), path, err))?;
        self.dir
            .write(&rel, data)
            .map_err(|err| host_error("write", path, err))
    }

    fn append(&self, path: &str, data: &[u8]) -> Result<()> {
        let rel = self.rel(path)?;
        self.dir
            .create_dir_all(parent(&rel))
            .map_err(|err| host_error(&format!("mkdir for {path}"), path, err))?;
        let mut file = self
            .dir
            .open_with(&rel, CapOpenOptions::new().append(true).create(true))
            .map_err(|err| host_error("append", path, err))?;
        file.write_all(data)
            .map_err(|err| host_error("append", path, err))
    }

    fn rename(&self, src: &str, dst: &str) -> Result<()> {
        let src_rel = self.rel(src)?;
        let dst_rel = self.rel(dst)?;
        self.dir
            .create_dir_all(parent(&dst_rel))
            .map_err(|err| host_error(&format!("mkdir for {dst}"), dst, err))?;
        self.dir
            .rename(&src_rel, &self.dir, &dst_rel)
            .map_err(|err| {
                if err.kind() == io::ErrorKind::NotFound {
                    refusal::not_found(src)
                } else {
                    host_error(&format!("rename {src} -> {dst}"), dst, err)
                }
            })
    }

    fn delete(&self, path: &str) -> Result<()> {
        let rel = self.rel(path)?;
        match self.dir.metadata(&rel) {
            Ok(meta) if meta.is_dir() => {
                return Err(refusal::validation(format!(
                    "workspace: {path} is a directory (use RemoveAll)"
                )));
            }
            Ok(_) => {}
            Err(err) if err.kind() == io::ErrorKind::NotFound => {}
            Err(err) => return Err(host_error("delete", path, err)),
        }
        match self.dir.remove_file(&rel) {
            Ok(()) => Ok(()),
            Err(err) if err.kind() == io::ErrorKind::NotFound => Ok(()),
            Err(err) => Err(host_error("delete", path, err)),
        }
    }

    fn remove_all(&self, path: &str) -> Result<()> {
        let Some(cleaned) = clean_path(path)? else {
            return Err(refusal::forbidden("workspace: refusing to remove root"));
        };
        let rel = join(&[&self.prefix, &cleaned]);
        // A final symlink is unlinked, never followed: the handle stats without
        // following, so `remove_all` on a link removes the link and leaves what it
        // pointed at -- core's rule, and the reason a link that escapes is still
        // removable.
        match self.dir.remove_dir_all(&rel) {
            Ok(()) => Ok(()),
            Err(err) if err.kind() == io::ErrorKind::NotFound => Ok(()),
            Err(err) => Err(host_error("remove all", path, err)),
        }
    }

    fn list(&self, dir: &str) -> Result<Vec<Entry>> {
        let rel = self.rel(dir)?;
        let entries = match self.dir.read_dir(&rel) {
            Ok(entries) => entries,
            Err(err) if err.kind() == io::ErrorKind::NotFound => return Ok(Vec::new()),
            Err(err) => return Err(host_error("list", dir, err)),
        };
        let mut names = Vec::new();
        for entry in entries {
            let entry = entry.map_err(|err| host_error("list", dir, err))?;
            let is_dir = entry
                .file_type()
                .map_err(|err| host_error("list", dir, err))?
                .is_dir();
            names.push(Entry {
                name: entry.file_name().to_string_lossy().into_owned(),
                is_dir,
            });
        }
        names.sort_by(|left, right| left.name.cmp(&right.name));
        Ok(names)
    }

    fn exists(&self, path: &str) -> Result<bool> {
        let rel = self.rel(path)?;
        match self.dir.metadata(&rel) {
            Ok(_) => Ok(true),
            Err(err) if err.kind() == io::ErrorKind::NotFound => Ok(false),
            Err(err) => Err(host_error("exists", path, err)),
        }
    }

    fn stat(&self, path: &str) -> Result<Meta> {
        let rel = self.rel(path)?;
        let meta = self.dir.metadata(&rel).map_err(|err| {
            if err.kind() == io::ErrorKind::NotFound {
                refusal::not_found(path)
            } else {
                host_error("stat", path, err)
            }
        })?;
        Ok(Meta {
            name: self.name_of(path),
            // A directory's size is the host's (an inode's blocks, or nothing at
            // all); the contract does not state it, and core's in-memory medium
            // reports zero, so the port reports zero.
            len: if meta.is_dir() { 0 } else { meta.len() },
            is_dir: meta.is_dir(),
        })
    }

    fn as_limited(&self) -> Option<&dyn LimitedReader> {
        Some(self)
    }

    fn capabilities(&self) -> Capabilities {
        Capabilities {
            atomic_rename: true,
            read_after_write: true,
            durable_on_write: false,
            distributed: false,
        }
    }
}

impl LimitedReader for LocalWorkspace {
    /// Opens the file *through* the handle and drains at most `max_bytes + 1`,
    /// so a file that grows concurrently cannot force an oversized allocation.
    fn read_limited(&self, path: &str, max_bytes: i64) -> Result<Vec<u8>> {
        if max_bytes <= 0 {
            return Err(refusal::validation(
                "workspace: ReadLimited maxBytes must be positive",
            ));
        }
        let rel = self.rel(path)?;
        let file = self.dir.open(&rel).map_err(|err| {
            if err.kind() == io::ErrorKind::NotFound {
                refusal::not_found(path)
            } else {
                host_error("read", path, err)
            }
        })?;
        let limit = u64::try_from(max_bytes).unwrap_or(0).saturating_add(1);
        let mut data = Vec::new();
        file.take(limit)
            .read_to_end(&mut data)
            .map_err(|err| host_error("read", path, err))?;
        if data.len() as u64 > u64::try_from(max_bytes).unwrap_or(0) {
            return Err(refusal::validation(format!(
                "workspace: {path} exceeds {max_bytes} bytes"
            )));
        }
        Ok(data)
    }
}

/// The parent of a `/`-separated relative path, for creating missing directories.
fn parent(rel: &str) -> &str {
    match rel.rfind('/') {
        Some(index) if index > 0 => &rel[..index],
        Some(_) => "/",
        None => ".",
    }
}

/// A failure the host reported, in the shape this backend reports them.
///
/// The sentence is core's (`workspace: <verb> <path>: <host>`); the host's part of
/// it is the platform's, which is why a conformance row over it asserts the
/// outcome and the kind, not the words.
fn host_error(verb: &str, path: &str, err: io::Error) -> Error {
    Error::new(
        ErrorKind::Internal,
        format!("workspace: {verb} {path}: {err}"),
    )
}

/// Whether `path` is `root` itself or beneath it, byte-exactly.
///
/// Used only to classify a refusal; the handle is the boundary.
fn within(path: &Path, root: &Path) -> bool {
    path == root || path.starts_with(root)
}

/// Resolves symlinks for the longest existing ancestor of `path`, then appends the
/// tail that does not exist yet.
///
/// `filepath.EvalSymlinks`'s behaviour, and like core's use of it, this is only for
/// deciding which refusal a prefix gets.
fn resolve_existing_prefix(path: &Path) -> io::Result<PathBuf> {
    match std::fs::canonicalize(path) {
        Ok(real) => Ok(real),
        Err(err) if err.kind() == io::ErrorKind::NotFound => {
            let (Some(parent), Some(name)) = (path.parent(), path.file_name()) else {
                return Ok(path.to_path_buf());
            };
            let resolved = resolve_existing_prefix(parent)?;
            Ok(resolved.join(name))
        }
        Err(err) => Err(err),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testing::{ok, refused};

    /// A temporary directory for one test, emptied first.
    fn scratch(name: &str) -> PathBuf {
        let mut path = std::env::temp_dir();
        path.push(format!("flowcraft-local-{name}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&path);
        path
    }

    #[test]
    fn a_missing_root_is_created_and_resolved() {
        let base = scratch("created");
        let requested = base.join("nested").join("root");
        let ws = ok(LocalWorkspace::new(&requested));
        // The display path is the resolved one, so a root reached through a
        // symlink does not hand out a path that another process could swap.
        let resolved = ok(std::fs::canonicalize(&requested)
            .map_err(|err| Error::new(ErrorKind::Internal, err.to_string())));
        assert_eq!(ws.root(), resolved);
        ok(ws.write("a.txt", b"a"));
        assert!(requested.join("a.txt").is_file());
        let _ = std::fs::remove_dir_all(&base);
    }

    #[test]
    fn an_empty_prefix_is_the_same_view() {
        let base = scratch("empty-prefix");
        let ws = ok(LocalWorkspace::new(&base));
        let same = ok(ws.sub(""));
        assert_eq!(same.root(), ws.root());
        ok(same.write("a.txt", b"a"));
        assert_eq!(ok(ws.read("a.txt")), b"a");
        let _ = std::fs::remove_dir_all(&base);
    }

    #[test]
    fn a_prefix_creates_the_directory_and_keeps_the_handle() {
        let base = scratch("prefix");
        let ws = ok(LocalWorkspace::new(&base));
        let view = ok(ws.sub("s/t"));
        assert!(base.join("s/t").is_dir(), "the view's directory exists");
        ok(view.write("f.txt", b"f"));
        // The parent sees it at the prefixed path, because both share the handle.
        assert_eq!(ok(ws.read("s/t/f.txt")), b"f");
        // And the view cannot climb back out of it.
        assert_eq!(
            refused(view.read("../a.txt")),
            "workspace: path traversal denied: ../a.txt"
        );
        let _ = std::fs::remove_dir_all(&base);
    }
}
