//! [`Scoped`]: the permission view -- a read deny-list and a write allow-list
//! over another workspace.
//!
//! The two directions are deliberately asymmetric, and that asymmetry is the
//! design rather than an oversight:
//!
//! - **Read is deny-only.** Everything is readable unless a `deny_read` pattern
//!   matches. A caller that forgets to configure anything keeps working, which is
//!   what makes the view safe to add to an existing deployment.
//! - **Write is allow-only.** Nothing is writable unless an `allow_write` pattern
//!   matches. A caller that forgets to configure anything can write nothing. The
//!   failure is loud and the default is closed, which is the right direction for
//!   the operation that can destroy state.
//! - **`mandatory_deny` covers both**, and is checked first, so a deployment
//!   cannot allow-list its way into a path the operator marked off limits.
//!
//! Every refusal builds a [`ViolationRecord`] and hands it to the logger *before*
//! the error is returned: a denied write is a security event, and a caller that
//! only sees an error would have to parse a sentence to find out what was
//! attempted. core's default logger warns through telemetry; the port has no
//! telemetry sink yet, so a view with no logger configured records nothing --
//! the record is still built, and the contract row is what carries it.
//!
//! # Matching happens on the cleaned path, refusing happens on the given one
//!
//! The patterns are matched against `clean(path)`, so `a/../b` is judged as `b`
//! -- otherwise a deny rule on `b` would be one `..` away from useless. The
//! refusal *text* and the violation record carry the path as the caller wrote it,
//! because that is what an operator needs to see. Traversal is not rejected here;
//! the wrapped workspace does that, and a scope that rejected it first would hide
//! which layer refused.

use std::sync::Arc;

use crate::clean::clean;
use crate::pattern::matches_any;
use crate::{Capabilities, Entry, LimitedReader, Meta, Result, Workspace, refusal};

/// One refused operation, as the logger receives it.
///
/// core's record carries a timestamp; the port does not, because the only clock
/// in the room is the caller's and two implementations cannot agree on it. What
/// is left is what the scope decided.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ViolationRecord {
    /// `read` or `write` -- the direction that was refused.
    pub operation: String,
    /// The path as the caller wrote it.
    pub path: String,
    /// Which rule refused: `mandatory deny`, `deny-read pattern matched`, or
    /// `not in allow-write list`.
    pub reason: String,
}

/// Receives a [`ViolationRecord`] for every refusal.
pub trait ViolationLogger: Send + Sync {
    /// Called once per refused operation, before the refusal is returned.
    fn log_violation(&self, record: &ViolationRecord);
}

/// The scope's configuration.
///
/// Built by method chain: `ScopedOptions::new().deny_read(["secrets/**"])`. The
/// three pattern lists append in call order, so a deployment can compose them
/// from several sources without one replacing another.
#[derive(Default)]
pub struct ScopedOptions {
    deny_read: Vec<String>,
    allow_write: Vec<String>,
    mandatory_deny: Vec<String>,
    logger: Option<Arc<dyn ViolationLogger>>,
}

impl ScopedOptions {
    /// No patterns and no logger: reads are unrestricted, writes are refused.
    pub fn new() -> Self {
        ScopedOptions::default()
    }

    /// Appends read-deny patterns.
    pub fn deny_read<I, S>(mut self, patterns: I) -> Self
    where
        I: IntoIterator<Item = S>,
        S: Into<String>,
    {
        self.deny_read.extend(patterns.into_iter().map(Into::into));
        self
    }

    /// Appends write-allow patterns.
    pub fn allow_write<I, S>(mut self, patterns: I) -> Self
    where
        I: IntoIterator<Item = S>,
        S: Into<String>,
    {
        self.allow_write
            .extend(patterns.into_iter().map(Into::into));
        self
    }

    /// Appends patterns that are refused in both directions.
    pub fn mandatory_deny<I, S>(mut self, patterns: I) -> Self
    where
        I: IntoIterator<Item = S>,
        S: Into<String>,
    {
        self.mandatory_deny
            .extend(patterns.into_iter().map(Into::into));
        self
    }

    /// Sets the logger that receives every refusal.
    pub fn logger(mut self, logger: Arc<dyn ViolationLogger>) -> Self {
        self.logger = Some(logger);
        self
    }

    /// The read-deny patterns, in the order they were added.
    pub fn deny_read_patterns(&self) -> &[String] {
        &self.deny_read
    }

    /// The write-allow patterns, in the order they were added.
    pub fn allow_write_patterns(&self) -> &[String] {
        &self.allow_write
    }

    /// The patterns refused in both directions.
    pub fn mandatory_deny_patterns(&self) -> &[String] {
        &self.mandatory_deny
    }
}

/// A workspace view that enforces a permission policy.
pub struct Scoped {
    inner: Box<dyn Workspace>,
    options: ScopedOptions,
}

impl Scoped {
    /// Wraps `inner` with `options`.
    pub fn new(inner: Box<dyn Workspace>, options: ScopedOptions) -> Self {
        Scoped { inner, options }
    }

    /// The wrapped workspace.
    ///
    /// Operations reached this way bypass the policy -- that is the point of
    /// asking for it, and the reason it is named rather than automatic.
    pub fn unwrap(self) -> Box<dyn Workspace> {
        self.inner
    }

    /// The rules this view was built with.
    pub fn options(&self) -> &ScopedOptions {
        &self.options
    }

    /// Refuses the read if the policy does, recording the violation.
    fn check_read(&self, path: &str) -> Result<()> {
        let cleaned = clean(path);
        if matches_any(&cleaned, &self.options.mandatory_deny) {
            self.log("read", path, "mandatory deny");
            return Err(refusal::access_denied(path, "mandatory deny"));
        }
        if matches_any(&cleaned, &self.options.deny_read) {
            self.log("read", path, "deny-read pattern matched");
            return Err(refusal::access_denied(path, "read denied"));
        }
        Ok(())
    }

    /// Refuses the write if the policy does, recording the violation.
    fn check_write(&self, path: &str) -> Result<()> {
        let cleaned = clean(path);
        if matches_any(&cleaned, &self.options.mandatory_deny) {
            self.log("write", path, "mandatory deny");
            return Err(refusal::access_denied(path, "mandatory deny"));
        }
        if !matches_any(&cleaned, &self.options.allow_write) {
            self.log("write", path, "not in allow-write list");
            return Err(refusal::access_denied(path, "write denied"));
        }
        Ok(())
    }

    /// Hands one refused operation to the logger, if there is one.
    fn log(&self, operation: &str, path: &str, reason: &str) {
        if let Some(logger) = &self.options.logger {
            logger.log_violation(&ViolationRecord {
                operation: operation.to_string(),
                path: path.to_string(),
                reason: reason.to_string(),
            });
        }
    }
}

impl Workspace for Scoped {
    fn read(&self, path: &str) -> Result<Vec<u8>> {
        self.check_read(path)?;
        self.inner.read(path)
    }

    fn write(&self, path: &str, data: &[u8]) -> Result<()> {
        self.check_write(path)?;
        self.inner.write(path, data)
    }

    fn append(&self, path: &str, data: &[u8]) -> Result<()> {
        self.check_write(path)?;
        self.inner.append(path, data)
    }

    fn rename(&self, src: &str, dst: &str) -> Result<()> {
        self.check_write(src)?;
        self.check_write(dst)?;
        self.inner.rename(src, dst)
    }

    fn delete(&self, path: &str) -> Result<()> {
        self.check_write(path)?;
        self.inner.delete(path)
    }

    fn remove_all(&self, path: &str) -> Result<()> {
        self.check_write(path)?;
        self.inner.remove_all(path)
    }

    fn list(&self, dir: &str) -> Result<Vec<Entry>> {
        self.check_read(dir)?;
        self.inner.list(dir)
    }

    fn exists(&self, path: &str) -> Result<bool> {
        self.check_read(path)?;
        self.inner.exists(path)
    }

    fn stat(&self, path: &str) -> Result<Meta> {
        self.check_read(path)?;
        self.inner.stat(path)
    }

    /// Answers `Some(self)` even when the backend cannot bound a read, exactly
    /// as core's wrapper has `ReadLimited` in its method set: [`crate::capped`]
    /// then gets a not-available refusal from here instead of silently
    /// materializing the file.
    fn as_limited(&self) -> Option<&dyn LimitedReader> {
        Some(self)
    }

    /// Scoping is a permission boundary, not a medium: the storage guarantees
    /// are the wrapped workspace's.
    fn capabilities(&self) -> Capabilities {
        self.inner.capabilities()
    }
}

impl LimitedReader for Scoped {
    fn read_limited(&self, path: &str, max_bytes: i64) -> Result<Vec<u8>> {
        self.check_read(path)?;
        match self.inner.as_limited() {
            Some(limited) => limited.read_limited(path, max_bytes),
            None => Err(refusal::not_available(
                "workspace: scoped workspace cannot perform bounded reads",
            )),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::mem::MemWorkspace;
    use crate::testing::{ok, refusal, refused};
    use std::sync::Mutex;

    #[derive(Default)]
    struct Recorder(Mutex<Vec<ViolationRecord>>);

    impl Recorder {
        /// The records so far. A poisoned lock is recovered: the recorder holds a
        /// `Vec` that a panicking test cannot have left torn.
        fn records(&self) -> Vec<ViolationRecord> {
            match self.0.lock() {
                Ok(records) => records.clone(),
                Err(poisoned) => poisoned.into_inner().clone(),
            }
        }
    }

    impl ViolationLogger for Recorder {
        fn log_violation(&self, record: &ViolationRecord) {
            match self.0.lock() {
                Ok(mut records) => records.push(record.clone()),
                Err(poisoned) => poisoned.into_inner().push(record.clone()),
            }
        }
    }

    fn view(options: ScopedOptions) -> (Scoped, Arc<Recorder>) {
        let recorder = Arc::new(Recorder::default());
        let inner = MemWorkspace::with_files([("secrets/key.txt", "s"), ("out/a.txt", "a")]);
        let view = Scoped::new(Box::new(inner), options.logger(recorder.clone()));
        (view, recorder)
    }

    #[test]
    fn read_is_deny_only_and_write_is_allow_only() {
        let (ws, log) = view(
            ScopedOptions::new()
                .deny_read(["secrets/**"])
                .allow_write(["out/**"]),
        );
        assert_eq!(
            refused(ws.read("secrets/key.txt")),
            "workspace: access denied: secrets/key.txt (read denied)"
        );
        ok(ws.read("out/a.txt"));
        ok(ws.write("out/b.txt", b"b"));
        assert_eq!(
            refused(ws.write("other/c.txt", b"c")),
            "workspace: access denied: other/c.txt (write denied)"
        );

        let records = log.records();
        assert_eq!(
            records,
            vec![
                ViolationRecord {
                    operation: "read".into(),
                    path: "secrets/key.txt".into(),
                    reason: "deny-read pattern matched".into()
                },
                ViolationRecord {
                    operation: "write".into(),
                    path: "other/c.txt".into(),
                    reason: "not in allow-write list".into()
                },
            ]
        );
    }

    #[test]
    fn mandatory_deny_is_checked_first_and_says_so() {
        let (ws, log) = view(
            ScopedOptions::new()
                .allow_write(["**"])
                .mandatory_deny(["secrets/**"]),
        );
        let err = refusal(ws.write("secrets/../secrets/x", b"x"));
        assert_eq!(
            err.to_string(),
            "workspace: access denied: secrets/../secrets/x (mandatory deny)"
        );
        assert_eq!(log.records()[0].reason, "mandatory deny");
    }

    #[test]
    fn matching_is_on_the_cleaned_path_and_the_text_on_the_given_one() {
        let (ws, _) = view(ScopedOptions::new().deny_read(["secrets/**"]));
        let err = refusal(ws.read("secrets/../secrets/key.txt"));
        assert_eq!(
            err.to_string(),
            "workspace: access denied: secrets/../secrets/key.txt (read denied)"
        );
    }

    #[test]
    fn traversal_is_left_to_the_wrapped_workspace() {
        let (ws, _) = view(ScopedOptions::new().allow_write(["**"]));
        let err = refusal(ws.read("../etc/passwd"));
        assert_eq!(
            err.to_string(),
            "workspace: path traversal denied: ../etc/passwd"
        );
    }

    #[test]
    fn a_bounded_read_fails_closed_when_the_backend_cannot_bound() {
        let (ws, _) = view(ScopedOptions::new());
        let err = refusal(crate::capped(&ws, "out/a.txt", 10));
        assert_eq!(
            err.to_string(),
            "workspace: scoped workspace cannot perform bounded reads"
        );
    }

    #[test]
    fn capabilities_are_the_wrapped_workspace_s() {
        let (ws, _) = view(ScopedOptions::new());
        assert!(ws.capabilities().atomic_rename);
        assert!(ws.capabilities().read_after_write);
        assert!(!ws.capabilities().durable_on_write);
    }
}
