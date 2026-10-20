//! [`Sub`]: a view rooted at a prefix of another workspace.
//!
//! Every path a caller hands a view is joined onto the prefix and forwarded. That
//! is the whole type, and it is what lets a subsystem be written against a
//! workspace root of its own without knowing where it sits: a memory index opened
//! at `sub/memory` sees `a.txt`, not `sub/memory/a.txt`.
//!
//! # Three shapes of answer, and this one
//!
//! core's `Sub` decides at construction what to build: a prefix of `""` returns
//! the inner workspace unchanged, a `*LocalWorkspace` becomes another local view
//! (sharing the parent's directory handle, so its `Root()` still answers a usable
//! path), and everything else becomes a generic prefix view. The port has only the
//! last shape for now -- the local view arrives with the local backend -- and the
//! first: an empty prefix still returns the inner workspace rather than a wrapper,
//! so `Sub(ws, "")` is `ws` and not a view that costs a join per call.
//!
//! core also *flattens* a chain: `Sub(Sub(base, "a"), "b")` becomes one view with
//! the prefix `a/b`. The port nests instead. Every operation answers the same,
//! because joining `b/x` onto `a` is joining `a/b/x` in one step; what differs is
//! that [`Sub::prefix`] reports this view's own segment rather than the joined
//! path, and that the local view's `Root()` (next slice) will only be reachable
//! from the outermost view that is one.
//!
//! # A bad prefix is a view that refuses everything
//!
//! Construction cannot fail in Rust the way it can in Go (a `Box<dyn Workspace>`
//! is returned either way), so a prefix that escapes the root becomes a
//! [`Refusing`] view: every operation answers one sentence, the one core builds
//! at construction -- `workspace sub: invalid prefix "../x": workspace: path
//! traversal denied: ../x`. Nesting one inside another keeps the sentence, which
//! is what core's `initErr` propagation is for.

use flowcraft_contract::error::{Error, ErrorKind};

use crate::clean::{clean_path, join};
use crate::{Capabilities, Entry, LimitedReader, Meta, Result, Workspace, refusal};

/// Wraps `inner` in a view rooted at `prefix`.
///
/// Returns `inner` unchanged for an empty prefix, and a [`Refusing`] view for a
/// prefix that is absolute or climbs out.
pub fn sub(inner: Box<dyn Workspace>, prefix: &str) -> Box<dyn Workspace> {
    match clean_path(prefix) {
        Err(err) => Box::new(Refusing::new(
            err.kind(),
            format!("workspace sub: invalid prefix {prefix:?}: {err}"),
        )),
        Ok(None) => inner,
        Ok(Some(cleaned)) => Box::new(Sub {
            inner,
            prefix: cleaned,
        }),
    }
}

/// A prefixed view of another workspace. Built with [`sub`].
pub struct Sub {
    inner: Box<dyn Workspace>,
    prefix: String,
}

impl Sub {
    /// The prefix this view joins onto every path.
    pub fn prefix(&self) -> &str {
        &self.prefix
    }

    /// The joined path for `path`.
    fn join(&self, path: &str) -> Result<String> {
        Ok(match clean_path(path)? {
            None => self.prefix.clone(),
            Some(cleaned) => join(&[&self.prefix, &cleaned]),
        })
    }
}

impl Workspace for Sub {
    fn read(&self, path: &str) -> Result<Vec<u8>> {
        self.inner.read(&self.join(path)?)
    }

    fn write(&self, path: &str, data: &[u8]) -> Result<()> {
        self.inner.write(&self.join(path)?, data)
    }

    fn append(&self, path: &str, data: &[u8]) -> Result<()> {
        self.inner.append(&self.join(path)?, data)
    }

    fn rename(&self, src: &str, dst: &str) -> Result<()> {
        let src = self.join(src)?;
        let dst = self.join(dst)?;
        self.inner.rename(&src, &dst)
    }

    fn delete(&self, path: &str) -> Result<()> {
        self.inner.delete(&self.join(path)?)
    }

    fn remove_all(&self, path: &str) -> Result<()> {
        // The root of a view cannot be removed, and the check has to happen
        // before the join: afterwards the prefix makes every path non-empty.
        if clean_path(path)?.is_none() {
            return Err(refusal::forbidden("workspace: refusing to remove root"));
        }
        self.inner.remove_all(&self.join(path)?)
    }

    fn list(&self, dir: &str) -> Result<Vec<Entry>> {
        self.inner.list(&self.join(dir)?)
    }

    fn exists(&self, path: &str) -> Result<bool> {
        self.inner.exists(&self.join(path)?)
    }

    fn stat(&self, path: &str) -> Result<Meta> {
        self.inner.stat(&self.join(path)?)
    }

    /// Answers `Some(self)` always: a prefixed view has no read of its own, so a
    /// backend that cannot bound is reported by *this* layer as not-available
    /// rather than falling back to a full read.
    fn as_limited(&self) -> Option<&dyn LimitedReader> {
        Some(self)
    }

    fn capabilities(&self) -> Capabilities {
        self.inner.capabilities()
    }
}

impl LimitedReader for Sub {
    fn read_limited(&self, path: &str, max_bytes: i64) -> Result<Vec<u8>> {
        let joined = self.join(path)?;
        match self.inner.as_limited() {
            Some(limited) => limited.read_limited(&joined, max_bytes),
            None => Err(refusal::not_available(
                "workspace: prefixed workspace cannot perform bounded reads",
            )),
        }
    }
}

/// A view whose construction failed: every operation answers the same refusal.
///
/// This is core's `subWorkspace{initErr: ..}`: a Go constructor that cannot return
/// an error has nowhere else to put one, and answering at construction would make
/// `Sub` a fallible call at every site that opens a well-known prefix.
pub struct Refusing {
    kind: ErrorKind,
    message: String,
}

impl Refusing {
    fn new(kind: ErrorKind, message: String) -> Self {
        Refusing { kind, message }
    }

    /// The refusal every operation answers.
    fn error(&self) -> Error {
        Error::new(self.kind, self.message.clone())
    }
}

impl Workspace for Refusing {
    fn read(&self, _path: &str) -> Result<Vec<u8>> {
        Err(self.error())
    }
    fn write(&self, _path: &str, _data: &[u8]) -> Result<()> {
        Err(self.error())
    }
    fn append(&self, _path: &str, _data: &[u8]) -> Result<()> {
        Err(self.error())
    }
    fn rename(&self, _src: &str, _dst: &str) -> Result<()> {
        Err(self.error())
    }
    fn delete(&self, _path: &str) -> Result<()> {
        Err(self.error())
    }
    fn remove_all(&self, _path: &str) -> Result<()> {
        Err(self.error())
    }
    fn list(&self, _dir: &str) -> Result<Vec<Entry>> {
        Err(self.error())
    }
    fn exists(&self, _path: &str) -> Result<bool> {
        Err(self.error())
    }
    fn stat(&self, _path: &str) -> Result<Meta> {
        Err(self.error())
    }
    fn as_limited(&self) -> Option<&dyn LimitedReader> {
        Some(self)
    }
}

impl LimitedReader for Refusing {
    fn read_limited(&self, _path: &str, _max_bytes: i64) -> Result<Vec<u8>> {
        Err(self.error())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::mem::MemWorkspace;
    use crate::testing::{ok, refusal, refused};

    fn base() -> Box<dyn Workspace> {
        Box::new(MemWorkspace::with_files([
            ("a/x.txt", "x"),
            ("a/b/y.txt", "y"),
            ("root.txt", "r"),
        ]))
    }

    #[test]
    fn an_empty_prefix_is_the_workspace_itself() {
        let ws = sub(base(), "");
        assert_eq!(ok(ws.read("root.txt")), b"r");
        let ws = sub(base(), ".");
        assert_eq!(ok(ws.read("root.txt")), b"r");
    }

    #[test]
    fn paths_are_joined_onto_the_prefix() {
        let ws = sub(base(), "a");
        assert_eq!(ok(ws.read("x.txt")), b"x");
        assert_eq!(ok(ws.read("b/y.txt")), b"y");
        assert_eq!(ok(ws.list("")).len(), 2);
        ok(ws.write("new/z.txt", b"z"));
        assert_eq!(ok(ws.read("new/z.txt")), b"z");
    }

    #[test]
    fn a_traversal_refusal_is_the_views_own() {
        let ws = sub(base(), "a");
        let err = refusal(ws.read("../root.txt"));
        assert_eq!(
            err.to_string(),
            "workspace: path traversal denied: ../root.txt"
        );
    }

    #[test]
    fn the_root_cannot_be_removed() {
        let ws = sub(base(), "a");
        assert_eq!(
            refused(ws.remove_all("")),
            "workspace: refusing to remove root"
        );
        assert_eq!(
            refused(ws.remove_all(".")),
            "workspace: refusing to remove root"
        );
        ok(ws.remove_all("b"));
        assert!(ok(ws.exists("x.txt")));
        assert!(!ok(ws.exists("b/y.txt")));
    }

    #[test]
    fn a_bad_prefix_refuses_everything_with_one_sentence() {
        let ws = sub(base(), "../escape");
        let want = "workspace sub: invalid prefix \"../escape\": workspace: path traversal denied: ../escape";
        for err in [
            refusal(ws.read("x")),
            refusal(ws.list("")),
            refusal(ws.exists("x")),
        ] {
            assert_eq!(err.to_string(), want);
        }
        // And nesting it does not lose the sentence.
        let nested = sub(ws, "inner");
        assert_eq!(refused(nested.read("x")), want);
    }

    #[test]
    fn a_bounded_read_fails_closed_at_this_layer() {
        let ws = sub(base(), "a");
        assert_eq!(
            refused(crate::capped(&ws, "x.txt", 10)),
            "workspace: prefixed workspace cannot perform bounded reads"
        );
        assert_eq!(ok(ws.read("x.txt")), b"x");
    }

    #[test]
    fn nesting_joins_both_prefixes() {
        let ws = sub(sub(base(), "a"), "b");
        assert_eq!(ok(ws.read("y.txt")), b"y");
        assert!(ws.write("../x.txt", b"x").is_err());
        ok(ws.write("z.txt", b"z"));
        assert_eq!(ok(ws.read("z.txt")), b"z");
    }

    #[test]
    fn capabilities_are_the_wrapped_workspace_s() {
        let ws = sub(base(), "a");
        assert!(ws.capabilities().atomic_rename);
    }
}
