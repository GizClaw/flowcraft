//! `core/workspace`: filesystem-shaped storage for agent state, and the rules
//! every backend agrees on.
//!
//! Higher layers -- sandboxed scripts, bridge files, the memory subsystem --
//! depend on [`Workspace`] and never on an implementation. This crate holds the
//! contract and the helpers written *in terms of* it ([`atomic_write`],
//! [`copy`], [`walk`], [`glob`], [`capped`]), plus the two views that wrap a
//! backend rather than store anything ([`Sub`], [`Scoped`]).
//!
//! # What every implementation promises
//!
//! - Paths are relative to the view. An absolute path, or one that climbs out
//!   with `..`, is refused with the path-traversal refusal -- and so is a path
//!   that *only looks* like one, because the check is lexical: `..a` is refused
//!   too (see [`clean_path`]).
//! - [`Workspace::write`] and [`Workspace::append`] create missing parent
//!   directories and fail when the path is an existing directory. Neither is
//!   atomic; [`atomic_write`] and [`Workspace::rename`] are how a finalized
//!   payload is published.
//! - [`Workspace::rename`] is the canonical publish operation. It answers
//!   not-found for a missing source, overwrites an existing destination, and
//!   implementations must be atomic where the medium allows it.
//! - [`Workspace::delete`] and [`Workspace::remove_all`] are idempotent:
//!   removing a missing path is success. `delete` is for files; a backend with
//!   a directory concept refuses a directory and points at `remove_all`.
//! - [`Workspace::list`] answers a directory's direct children, sorted by
//!   name. A missing or empty directory answers an empty list, not an error.
//! - [`Workspace::read`], [`Workspace::stat`] and the source of `rename`
//!   answer not-found for a missing path.
//!
//! # The port's shape, and why
//!
//! **Blocking, no `context`.** core threads a `context.Context` through every
//! method, but these are file operations: the context only ever reaches the
//! telemetry sink, never a cancellation check. The port keeps the trait
//! synchronous and leaves the async boundary to its caller, which is also what
//! makes `dyn Workspace` useful -- an `async fn` in a trait is not
//! object-safe without a boxed adapter, and this trait exists to be boxed.
//!
//! **Optional interfaces became default methods.** core has `LimitedReader`,
//! `CapabilityReporter` and `io.Closer` as type assertions beside the interface.
//! Here the first two are defaulted trait methods: [`Workspace::as_limited`]
//! (whose default answer is `None`) and [`Workspace::capabilities`] (whose
//! default is the conservative all-false answer). `Close` is [`Drop`].
//!
//! `as_limited` returns *self* rather than being a plain method on purpose.
//! core's `Capped` asks a *type* question -- "does this backend implement
//! `LimitedReader`?" -- and the answer decides which of two refusal sentences a
//! caller sees, so it cannot be collapsed into "call it and see". A wrapper view
//! ([`Scoped`], [`Sub`]) answers `Some` even when the backend underneath does
//! not, exactly as core's wrapper types have the method in their method set, and
//! the wrapper then fails closed with not-available.
//!
//! **Paths are strings, and they are Unix paths.** Workspace paths are a
//! portable namespace: `/` separates components on every platform, and a host
//! separator would make a path that comes back out of [`walk`] unusable as
//! input. Core's `foldCase` lower-cases on Windows; the port's targets for this
//! crate are case-sensitive filesystems, so [`fold_case`] is the identity, and
//! Windows is its own slice (`docs/plans/2026-10-15-rust-core-port.md` §12).
//! Behaviour that depends on Windows spelling -- backslashes being separators,
//! `MoveFileEx` refusing to replace a directory -- is therefore not here.
//!
//! # What is not ported
//!
//! `resource.go` (the `workspace.Local` deploy resource: kind, settings and
//! factory) belongs to the deploy-assembly slice, and `context.go`
//! (`WithWorkspace` / `WorkspaceFrom`) to whatever ends up owning execution
//! context: the plan is an explicit `ExecCtx` field, not a task-local. The
//! local backend is [`local::LocalWorkspace`]: `os.Root`'s containment is taken
//! from `cap-std`'s `Dir`, which is a directory handle with per-component
//! resolution beneath it -- the same primitive, so the rules at the edges are
//! the same ones (`docs/plans/2026-10-15-rust-core-port.md` §4.14).
//!
//! [`mem::MemWorkspace`] is the port's own: core has no in-memory workspace in
//! this package, and the helpers need a medium that is not a filesystem to be
//! asked about cleanly (see its docs).

#![deny(missing_docs)]

mod atomic;
mod capped;
mod clean;
pub mod local;
pub mod mem;
mod pattern;
mod scoped;
mod sub;
#[cfg(test)]
mod testing;
mod walk;

pub use atomic::{atomic_write, copy};
pub use capped::capped;
pub use clean::{base, clean, clean_path, dir, fold_case, join};
pub use local::LocalWorkspace;
pub use pattern::matches_any;
pub use scoped::{Scoped, ScopedOptions, ViolationLogger, ViolationRecord};
pub use sub::{Refusing, Sub, sub};
pub use walk::{GlobalMatch, Step, glob, glob_detailed, walk};

use flowcraft_contract::error::{Error, ErrorKind};

/// One directory entry, as [`Workspace::list`] reports it.
///
/// Metadata is not carried: a caller that needs it asks [`Workspace::stat`],
/// which is what keeps `list` cheap on object stores and keeps this type
/// comparable in the conformance dump.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Entry {
    /// The entry's name, without any parent path.
    pub name: String,
    /// Whether the entry is a directory.
    pub is_dir: bool,
}

impl Entry {
    /// An entry for a directory.
    pub fn dir(name: impl Into<String>) -> Self {
        Entry {
            name: name.into(),
            is_dir: true,
        }
    }

    /// An entry for a file.
    pub fn file(name: impl Into<String>) -> Self {
        Entry {
            name: name.into(),
            is_dir: false,
        }
    }
}

/// What [`Workspace::stat`] answers.
///
/// core returns `fs.FileInfo`, whose surface is larger than any caller reads
/// and whose `Sys` field is the host's. The port carries what the contract can
/// state: the name, the size in bytes, and whether it is a directory.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Meta {
    /// The final element of the path that was stat-ed.
    pub name: String,
    /// The size in bytes (zero for a directory, as core reports it).
    pub len: u64,
    /// Whether the path is a directory.
    pub is_dir: bool,
}

/// The storage characteristics of a backend.
///
/// Every field defaults to `false`, the conservative reading: an adapter that
/// is not told otherwise must assume the weakest guarantees. That is why adding
/// a field is additive rather than a break.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct Capabilities {
    /// `rename` succeeds or fails as one observable step.
    pub atomic_rename: bool,
    /// A successful write is immediately visible to this client.
    pub read_after_write: bool,
    /// A successful write reached stable storage before returning.
    pub durable_on_write: bool,
    /// More than one process can open the same workspace at once.
    pub distributed: bool,
}

/// A backend that can bound a single read.
///
/// Reachable through [`Workspace::as_limited`], never by asking a value whether
/// it "supports" bounded reads: the point is that an oversized file is refused
/// *at the read layer*, so it never lands in memory whole. [`capped`] prefers
/// this and falls back to a full read for backends that cannot.
pub trait LimitedReader {
    /// Reads `path`, materializing no more than `max_bytes`, and refuses a
    /// larger file.
    fn read_limited(&self, path: &str, max_bytes: i64) -> Result<Vec<u8>>;
}

/// Filesystem-shaped storage over a sandboxed tree.
///
/// The promises are the module documentation's; implementations differ in
/// medium, not in rules. `Send + Sync` because a view is shared: a turn's
/// tools, the sandbox and a memory index all reach the same tree.
pub trait Workspace: Send + Sync {
    /// The full contents of `path`, or the not-found refusal.
    fn read(&self, path: &str) -> Result<Vec<u8>>;

    /// Stores `data` at `path`, creating missing parents.
    ///
    /// Not atomic: use [`atomic_write`] when a reader must never see a partial
    /// payload. Fails when `path` is an existing directory.
    fn write(&self, path: &str, data: &[u8]) -> Result<()>;

    /// Appends `data` to `path`, creating the file and missing parents.
    ///
    /// Not atomic, and not for publishing a finalized payload.
    fn append(&self, path: &str, data: &[u8]) -> Result<()>;

    /// Moves `src` to `dst` within the view.
    ///
    /// Not-found for a missing source; overwriting an existing destination is
    /// allowed. Directory renames are outside the contract.
    fn rename(&self, src: &str, dst: &str) -> Result<()>;

    /// Removes the file at `path`; idempotent for a missing path.
    ///
    /// Never removes directory contents: a backend with a directory concept
    /// refuses a directory and points at [`Workspace::remove_all`].
    fn delete(&self, path: &str) -> Result<()>;

    /// Recursively removes the tree at `path`; idempotent for a missing path.
    ///
    /// Removing the view's root is refused.
    fn remove_all(&self, path: &str) -> Result<()>;

    /// The direct children of `dir`, sorted by name.
    ///
    /// A missing or empty directory answers an empty list with no error, and
    /// entries synthesized from nested paths report `is_dir`.
    fn list(&self, dir: &str) -> Result<Vec<Entry>>;

    /// Whether `path` exists; a missing path is `Ok(false)`, not an error.
    fn exists(&self, path: &str) -> Result<bool>;

    /// Metadata for `path`, or the not-found refusal.
    fn stat(&self, path: &str) -> Result<Meta>;

    /// This backend as a [`LimitedReader`], when it can bound a read.
    ///
    /// The default is "cannot", which is what makes the weaker of [`capped`]'s
    /// two paths reachable. A backend that *can* returns `Some(self)`; a view
    /// that forwards returns `Some(self)` too, even when the backend underneath
    /// cannot, so the refusal stays at the wrapper.
    fn as_limited(&self) -> Option<&dyn LimitedReader> {
        None
    }

    /// The backend's [`Capabilities`]; the default is the conservative answer.
    fn capabilities(&self) -> Capabilities {
        Capabilities::default()
    }
}

/// A boxed workspace is a workspace.
///
/// Every method forwards, including the two that are not part of the storage
/// surface: without the [`Workspace::as_limited`] forwarding a `Box<dyn
/// Workspace>` would answer "cannot bound a read" for a backend that can, which
/// is exactly the silent downgrade [`capped`] exists to avoid.
impl<T: Workspace + ?Sized> Workspace for Box<T> {
    fn read(&self, path: &str) -> Result<Vec<u8>> {
        (**self).read(path)
    }
    fn write(&self, path: &str, data: &[u8]) -> Result<()> {
        (**self).write(path, data)
    }
    fn append(&self, path: &str, data: &[u8]) -> Result<()> {
        (**self).append(path, data)
    }
    fn rename(&self, src: &str, dst: &str) -> Result<()> {
        (**self).rename(src, dst)
    }
    fn delete(&self, path: &str) -> Result<()> {
        (**self).delete(path)
    }
    fn remove_all(&self, path: &str) -> Result<()> {
        (**self).remove_all(path)
    }
    fn list(&self, dir: &str) -> Result<Vec<Entry>> {
        (**self).list(dir)
    }
    fn exists(&self, path: &str) -> Result<bool> {
        (**self).exists(path)
    }
    fn stat(&self, path: &str) -> Result<Meta> {
        (**self).stat(path)
    }
    fn as_limited(&self) -> Option<&dyn LimitedReader> {
        (**self).as_limited()
    }
    fn capabilities(&self) -> Capabilities {
        (**self).capabilities()
    }
}

/// The crate's result type: a [`flowcraft_contract::Error`], whose text is
/// core's sentence for the same refusal.
pub type Result<T> = std::result::Result<T, Error>;

/// The three refusals the contract names, in core's words.
///
/// Each is a classified [`Error`]: a traversal or a denial is `Forbidden`, a
/// missing path is `NotFound`. Callers match [`Error::kind`] rather than the
/// sentence, and the sentence stays core's so an operator's log does not change
/// between the two implementations.
pub mod refusal {
    use super::{Error, ErrorKind};

    /// The sentence core wraps every escaping path with.
    pub const TRAVERSAL: &str = "workspace: path traversal denied";
    /// The sentence core wraps every policy refusal with.
    pub const ACCESS_DENIED: &str = "workspace: access denied";
    /// The sentence core wraps every missing path with.
    pub const NOT_FOUND: &str = "workspace: not found";

    /// A path outside the view, spelled as core spells it.
    pub fn path_traversal(path: &str) -> Error {
        Error::new(ErrorKind::Forbidden, format!("{TRAVERSAL}: {path}"))
    }

    /// A refusal with core's reason appended in parentheses.
    pub fn access_denied(path: &str, why: &str) -> Error {
        Error::new(
            ErrorKind::Forbidden,
            format!("{ACCESS_DENIED}: {path} ({why})"),
        )
    }

    /// A missing path.
    pub fn not_found(path: &str) -> Error {
        Error::new(ErrorKind::NotFound, format!("{NOT_FOUND}: {path}"))
    }

    /// A refusal core builds with `Validationf`, carrying core's sentence.
    pub fn validation(message: impl Into<String>) -> Error {
        Error::new(ErrorKind::Validation, message)
    }

    /// A capability the wrapped backend does not have.
    pub fn not_available(message: impl Into<String>) -> Error {
        Error::new(ErrorKind::NotAvailable, message)
    }

    /// A refusal core builds with `Forbiddenf`, carrying core's sentence.
    pub fn forbidden(message: impl Into<String>) -> Error {
        Error::new(ErrorKind::Forbidden, message)
    }
}
