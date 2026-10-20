//! An in-memory backend: the medium the helper contract is written against.
//!
//! core has no in-memory `Workspace` in this package (its backends are the local
//! tree, the scoped and prefixed views, and object stores implemented elsewhere),
//! so this type is the port's own. It is here for two reasons:
//!
//! - The helpers -- [`crate::walk`], [`crate::glob`], [`crate::capped`],
//!   [`crate::atomic_write`], [`crate::Sub`], [`crate::Scoped`] -- are all
//!   written against the trait, and a *flat map of files* is the sharpest way to
//!   ask what they do: directories are only what the paths imply, so a `list`
//!   that forgets to synthesize one is caught by construction.
//! - It gives the conformance rows a medium that is not a filesystem, which is
//!   what makes them portable: no temporary directory, no platform sorting, no
//!   permission bits. The Go oracle writes the same map-shaped backend into its
//!   own package, and the two must agree for a row to pass.
//!
//! It is not a production backend: no atomic durability, no size limits, no
//! cleanup. Its `Capabilities` are `atomic_rename` and `read_after_write` (both
//! true of a map under a lock) and nothing else.

use std::collections::BTreeMap;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, RwLock};

use flowcraft_contract::error::{Error, ErrorKind};

use crate::clean::{base, clean_path};
use crate::refusal;
use crate::{Capabilities, Entry, Meta, Result, Workspace};

/// A workspace held in a map of path to bytes.
///
/// Paths are stored cleaned, so `a/b` and `a//b` are one entry. Directories are
/// implicit: a path is a directory when some stored file lives under it.
///
/// Cloning is cheap and shares the contents: a [`crate::Scoped`] or [`crate::Sub`]
/// view is built over a clone of the same medium, and the caller keeps observing
/// it through the original.
#[derive(Debug, Clone, Default)]
pub struct MemWorkspace {
    inner: Arc<Inner>,
}

/// The shared half of [`MemWorkspace`].
#[derive(Debug, Default)]
struct Inner {
    files: RwLock<BTreeMap<String, Vec<u8>>>,
    refuse_rename: AtomicBool,
    limited: AtomicBool,
}

impl MemWorkspace {
    /// An empty workspace.
    pub fn new() -> Self {
        MemWorkspace::default()
    }

    /// A workspace holding `files`, written in iteration order.
    pub fn with_files<I, P, D>(files: I) -> Self
    where
        I: IntoIterator<Item = (P, D)>,
        P: AsRef<str>,
        D: AsRef<[u8]>,
    {
        let ws = MemWorkspace::new();
        for (path, data) in files {
            match ws.write(path.as_ref(), data.as_ref()) {
                Ok(()) => {}
                Err(err) => panic!("a fixture path is valid: {err}"),
            }
        }
        ws
    }

    /// Makes every [`Workspace::rename`] fail, as a store that cannot rename
    /// would, so the caller's error path can be observed.
    pub fn refuse_rename(&self, refuse: bool) {
        self.inner.refuse_rename.store(refuse, Ordering::SeqCst);
    }

    /// Makes the medium answer bounded reads.
    ///
    /// The flag is the port's stand-in for Go having two types (`memWorkspace`
    /// and `limitedWorkspace`): [`Workspace::as_limited`] asks a type-level
    /// question, and a test that wants the "yes" answer has to be able to say so.
    pub fn sync_limited_reads(&self, limited: bool) {
        self.inner.limited.store(limited, Ordering::SeqCst);
    }

    /// The map, borrowed for reading.
    ///
    /// A lock is poisoned when a thread panicked while holding it, and every
    /// mutation here is a single `insert` or `retain` under the write lock, so
    /// the map cannot be left torn: recovering the guard is the honest answer for
    /// a backend whose whole state is one map.
    fn rows(&self) -> std::sync::RwLockReadGuard<'_, BTreeMap<String, Vec<u8>>> {
        match self.inner.files.read() {
            Ok(guard) => guard,
            Err(poisoned) => poisoned.into_inner(),
        }
    }

    /// The map, borrowed for writing. See [`MemWorkspace::rows`].
    fn rows_mut(&self) -> std::sync::RwLockWriteGuard<'_, BTreeMap<String, Vec<u8>>> {
        match self.inner.files.write() {
            Ok(guard) => guard,
            Err(poisoned) => poisoned.into_inner(),
        }
    }

    /// Every file, sorted by path: the snapshot a row compares.
    pub fn snapshot(&self) -> BTreeMap<String, Vec<u8>> {
        self.rows().clone()
    }

    /// The stored paths, sorted.
    pub fn paths(&self) -> Vec<String> {
        self.rows().keys().cloned().collect()
    }

    /// Whether some stored file lives under `path`.
    fn is_dir(&self, path: &str) -> bool {
        let prefix = format!("{path}/");
        self.rows().keys().any(|key| key.starts_with(&prefix))
    }

    /// The normalized storage key for `path`; `""` is the root.
    ///
    /// `""`, `"."` and `"./"` all name the root here. That is this backend's
    /// rule rather than a rule of the trait (a real backend resolves the three
    /// the same way, but only after `Clean`); it is written down because the Go
    /// oracle's backend implements the same one, and a row that touched the root
    /// would notice a difference.
    fn key(path: &str) -> Result<String> {
        Ok(match clean_path(path)? {
            Some(cleaned) if cleaned != "." => cleaned,
            _ => String::new(),
        })
    }

    /// The refusal a write to the root gets.
    fn is_root(error: &str) -> Error {
        refusal::validation(format!("mem: {error} is a directory"))
    }
}

impl Workspace for MemWorkspace {
    fn read(&self, path: &str) -> Result<Vec<u8>> {
        let key = Self::key(path)?;
        match self.rows().get(&key) {
            Some(data) => Ok(data.clone()),
            None => Err(refusal::not_found(path)),
        }
    }

    fn write(&self, path: &str, data: &[u8]) -> Result<()> {
        let key = Self::key(path)?;
        if key.is_empty() || self.is_dir(&key) {
            return Err(Self::is_root(path));
        }
        self.rows_mut().insert(key, data.to_vec());
        Ok(())
    }

    fn append(&self, path: &str, data: &[u8]) -> Result<()> {
        let key = Self::key(path)?;
        if key.is_empty() || self.is_dir(&key) {
            return Err(Self::is_root(path));
        }
        let mut files = self.rows_mut();
        files.entry(key).or_default().extend_from_slice(data);
        Ok(())
    }

    fn rename(&self, src: &str, dst: &str) -> Result<()> {
        let src_key = Self::key(src)?;
        let dst_key = Self::key(dst)?;
        if self.inner.refuse_rename.load(Ordering::SeqCst) {
            return Err(Error::new(ErrorKind::Internal, "fixture: rename refused"));
        }
        let mut files = self.rows_mut();
        match files.remove(&src_key) {
            Some(data) => {
                files.insert(dst_key, data);
                Ok(())
            }
            None => Err(refusal::not_found(src)),
        }
    }

    fn delete(&self, path: &str) -> Result<()> {
        let key = Self::key(path)?;
        let mut files = self.rows_mut();
        if files.remove(&key).is_some() {
            return Ok(());
        }
        drop(files);
        if key.is_empty() || self.is_dir(&key) {
            return Err(Self::is_root(path));
        }
        Ok(())
    }

    fn remove_all(&self, path: &str) -> Result<()> {
        let cleaned = Self::key(path)?;
        if cleaned.is_empty() {
            // The medium's own refusal, spelled differently from `crate::Sub`'s so
            // a row says which layer answered.
            return Err(refusal::forbidden("mem: refusing to remove root"));
        }
        let prefix = format!("{cleaned}/");
        self.rows_mut()
            .retain(|key, _| key != &cleaned && !key.starts_with(&prefix));
        Ok(())
    }

    fn list(&self, dir: &str) -> Result<Vec<Entry>> {
        let cleaned = Self::key(dir)?;
        let prefix = if cleaned.is_empty() {
            String::new()
        } else {
            format!("{cleaned}/")
        };

        let files = self.rows();
        let mut entries: BTreeMap<String, bool> = BTreeMap::new();
        for key in files.keys() {
            let Some(rest) = key.strip_prefix(prefix.as_str()) else {
                continue;
            };
            if rest.is_empty() {
                continue;
            }
            match rest.find('/') {
                Some(i) => {
                    // An implicit directory: it wins over a same-named file.
                    entries.insert(rest[..i].to_string(), true);
                }
                None => {
                    entries.entry(rest.to_string()).or_insert(false);
                }
            }
        }
        Ok(entries
            .into_iter()
            .map(|(name, is_dir)| Entry { name, is_dir })
            .collect())
    }

    fn exists(&self, path: &str) -> Result<bool> {
        let cleaned = Self::key(path)?;
        if cleaned.is_empty() {
            return Ok(true);
        }
        if self.rows().contains_key(&cleaned) {
            return Ok(true);
        }
        Ok(self.is_dir(&cleaned))
    }

    fn stat(&self, path: &str) -> Result<Meta> {
        let cleaned = Self::key(path)?;
        if cleaned.is_empty() {
            return Ok(Meta {
                name: "/".to_string(),
                len: 0,
                is_dir: true,
            });
        }
        let files = self.rows();
        if let Some(data) = files.get(&cleaned) {
            return Ok(Meta {
                name: base(&cleaned),
                len: data.len() as u64,
                is_dir: false,
            });
        }
        drop(files);
        if self.is_dir(&cleaned) {
            return Ok(Meta {
                name: base(&cleaned),
                len: 0,
                is_dir: true,
            });
        }
        Err(refusal::not_found(path))
    }

    fn capabilities(&self) -> Capabilities {
        Capabilities {
            atomic_rename: true,
            read_after_write: true,
            durable_on_write: false,
            distributed: false,
        }
    }

    /// `Some(self)` only when [`MemWorkspace::sync_limited_reads`] said so.
    fn as_limited(&self) -> Option<&dyn crate::LimitedReader> {
        if self.inner.limited.load(Ordering::SeqCst) {
            Some(self)
        } else {
            None
        }
    }
}

impl crate::LimitedReader for MemWorkspace {
    fn read_limited(&self, path: &str, max_bytes: i64) -> Result<Vec<u8>> {
        let key = Self::key(path)?;
        match self.rows().get(&key) {
            Some(data) if data.len() as i64 > max_bytes => Err(refusal::validation(format!(
                "workspace: {path} exceeds {max_bytes} bytes"
            ))),
            Some(data) => Ok(data.clone()),
            None => Err(refusal::not_found(path)),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testing::{ok, refusal, refused};

    #[test]
    fn directories_are_only_what_the_paths_imply() {
        let ws = MemWorkspace::with_files([("a/b/c.txt", "x"), ("a/d.txt", "y"), ("e.txt", "z")]);
        let root = ok(ws.list(""));
        assert_eq!(root, vec![Entry::dir("a"), Entry::file("e.txt")]);
        assert_eq!(
            ok(ws.list("a")),
            vec![Entry::dir("b"), Entry::file("d.txt")]
        );
        assert_eq!(ok(ws.list("a/b")), vec![Entry::file("c.txt")]);
        assert_eq!(ok(ws.list("missing")), vec![]);
        assert_eq!(ok(ws.list("e.txt")), vec![]);
    }

    #[test]
    fn a_file_cannot_be_a_parent() {
        let ws = MemWorkspace::with_files([("a/b.txt", "x")]);
        let err = refusal(ws.write("a", b"y"));
        assert_eq!(err.to_string(), "mem: a is a directory");
        assert_eq!(ok(ws.read("a/b.txt")), b"x");
    }

    #[test]
    fn paths_are_stored_cleaned() {
        let ws = MemWorkspace::new();
        ok(ws.write("./a//b.txt", b"x"));
        assert_eq!(ws.paths(), vec!["a/b.txt"]);
        assert_eq!(ok(ws.read("a/./b.txt")), b"x");
    }

    #[test]
    fn delete_and_remove_all_are_idempotent() {
        let ws = MemWorkspace::with_files([("a/b/c.txt", "x")]);
        ok(ws.delete("a/b/c.txt"));
        ok(ws.delete("a/b/c.txt"));
        ok(ws.delete("nothing"));
        ok(ws.remove_all("a/b"));
        assert!(ws.paths().is_empty());
        ok(ws.remove_all("a/b"));
        assert_eq!(refused(ws.remove_all("")), "mem: refusing to remove root");
    }

    #[test]
    fn stat_reports_the_two_shapes() {
        let ws = MemWorkspace::with_files([("a/b.txt", "hello")]);
        assert_eq!(
            ok(ws.stat("a/b.txt")),
            Meta {
                name: "b.txt".into(),
                len: 5,
                is_dir: false
            }
        );
        assert_eq!(
            ok(ws.stat("a")),
            Meta {
                name: "a".into(),
                len: 0,
                is_dir: true
            }
        );
        assert_eq!(refused(ws.stat("a/none")), "workspace: not found: a/none");
    }
}
