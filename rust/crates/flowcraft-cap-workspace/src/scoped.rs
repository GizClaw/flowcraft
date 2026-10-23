//! Policy decoration: glob rules over any provider.
//!
//! This is the provider-side half of access control. The other half is the
//! grant's scope (see [`crate::scope`]), which is signed into a token and
//! enforced at the endpoint's trust boundary. Here the rules live inside the
//! process, next to the data, and apply no matter how the caller reached the
//! provider: a local handle, a decorator, or a future transport.
//!
//! # The two modes
//!
//! Reads are **deny-only**: everything is readable unless a rule names it.
//! Writes are **allow-only**: nothing is writable unless a rule names it, so an
//! empty `allow_write` means "read-only provider". `mandatory_deny` applies to
//! both and always wins.
//!
//! This asymmetry is on purpose and is the shape the Go side settled on: a typo
//! in a deny list leaves a file readable, a typo in an allow list leaves it
//! unwritable. Failing closed for writes and open for reads is the ordering
//! where a mistake costs availability rather than confidentiality.
//!
//! # Why a decorator and not a field on the local provider
//!
//! Composition is the graph's job: [`ScopedFactory`] declares a `workspace`
//! dependency, so the same policy wraps a local directory, a remote endpoint
//! view, or a prefixed view ([`crate::sub`]) without the provider knowing.
//! Carrying it inside `local` would have made every other provider grow the same
//! field.

use std::sync::Arc;
use std::time::SystemTime;

use async_trait::async_trait;
use flowcraft_error::{Error, Result};
use flowcraft_resource::Kind;
use flowcraft_resource::{BoxFuture, BuildCtx, Factory, Phase, Spec, require};
use serde::Deserialize;
use serde_json::Value;

use crate::contract::{CONTRACT, Capabilities, Entry, Handle, KIND, Stat, Workspace};
use crate::pattern;

/// Which operation a violation record is about.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Operation {
    /// A read-shaped call: `read`, `read_limited`, `list`, `stat`, `exists`.
    Read,
    /// A write-shaped call: `write`, `append`, `rename`, `delete`,
    /// `remove_all`.
    Write,
}

impl Operation {
    /// The stable spelling used in a violation record.
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Read => "read",
            Self::Write => "write",
        }
    }
}

/// One refused operation, for audit.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ViolationRecord {
    /// When the operation was refused.
    pub time: SystemTime,
    /// The operation class.
    pub operation: Operation,
    /// The path the caller named, as written.
    pub path: String,
    /// Why it was refused, in a fixed vocabulary.
    pub reason: &'static str,
}

/// receives violation records.
///
/// The default sink logs at `warn`; a deployment that has to prove an attempted
/// read was refused installs its own, the same seam the Go side exposes as
/// `ViolationLogger`.
pub trait ViolationSink: Send + Sync {
    /// Record one refusal. Called synchronously on the refusing task, so an
    /// implementation must not block.
    fn record(&self, record: &ViolationRecord);
}

/// The default sink: one `warn` record per refusal.
#[derive(Debug, Clone, Copy, Default)]
pub struct LogSink;

impl ViolationSink for LogSink {
    fn record(&self, record: &ViolationRecord) {
        tracing::warn!(
            op = record.operation.as_str(),
            path = %record.path,
            reason = record.reason,
            "workspace: access violation"
        );
    }
}

/// The compiled rules.
#[derive(Debug)]
pub struct Policy {
    deny_read: globset::GlobSet,
    allow_write: globset::GlobSet,
    mandatory_deny: globset::GlobSet,
}

impl Policy {
    /// Compile the three rule lists.
    ///
    /// # Errors
    ///
    /// An invalid pattern is [`ErrorKind::Validation`](flowcraft_error::ErrorKind::Validation),
    /// so a typo fails [`Plan`](flowcraft_resource::Plan) instead of the first
    /// request that happens to hit the rule.
    pub fn new(
        deny_read: &[String],
        allow_write: &[String],
        mandatory_deny: &[String],
    ) -> Result<Self> {
        Ok(Self {
            deny_read: pattern::compile_set(deny_read)?,
            allow_write: pattern::compile_set(allow_write)?,
            mandatory_deny: pattern::compile_set(mandatory_deny)?,
        })
    }
}

/// A workspace that refuses paths its [`Policy`] does not allow.
pub struct ScopedWorkspace {
    inner: Handle,
    policy: Policy,
    sink: Arc<dyn ViolationSink>,
}

impl std::fmt::Debug for ScopedWorkspace {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("workspace.ScopedWorkspace")
            .finish_non_exhaustive()
    }
}

impl ScopedWorkspace {
    /// Wrap a provider in a policy. Refusals are logged.
    #[must_use]
    pub fn new(inner: Handle, policy: Policy) -> Self {
        Self {
            inner,
            policy,
            sink: Arc::new(LogSink),
        }
    }

    /// Replace the violation sink.
    #[must_use]
    pub fn with_sink(mut self, sink: Arc<dyn ViolationSink>) -> Self {
        self.sink = sink;
        self
    }

    /// The provider this policy filters.
    ///
    /// The counterpart of the Go side's `Unwrap`: a caller that needs
    /// provider-specific extras (a local root path, say) recovers them from the
    /// handle it wrapped. Operations on the returned handle **bypass this
    /// policy** — that is the point of the accessor, and the reason the Go side
    /// documents the same caveat on `Unwrap`.
    #[must_use]
    pub fn provider(&self) -> Handle {
        self.inner.clone()
    }

    fn check_read(&self, path: &str) -> Result<()> {
        let normalized = pattern::normalize(path);
        if self.policy.mandatory_deny.is_match(&normalized) {
            return Err(self.deny(Operation::Read, path, "mandatory deny"));
        }
        if self.policy.deny_read.is_match(&normalized) {
            return Err(self.deny(Operation::Read, path, "deny-read pattern matched"));
        }
        Ok(())
    }

    fn check_write(&self, path: &str) -> Result<()> {
        let normalized = pattern::normalize(path);
        if self.policy.mandatory_deny.is_match(&normalized) {
            return Err(self.deny(Operation::Write, path, "mandatory deny"));
        }
        if !self.policy.allow_write.is_match(&normalized) {
            return Err(self.deny(Operation::Write, path, "not in allow-write list"));
        }
        Ok(())
    }

    fn deny(&self, operation: Operation, path: &str, reason: &'static str) -> Error {
        self.sink.record(&ViolationRecord {
            time: SystemTime::now(),
            operation,
            path: path.to_owned(),
            reason,
        });
        // The wording mirrors the Go side's `ErrAccessDenied` wrap, so an
        // operator switching a deployment between the two trees greps once.
        Error::forbidden(format!("workspace: access denied: {path} ({reason})"))
    }
}

#[async_trait]
impl Workspace for ScopedWorkspace {
    async fn capabilities(&self) -> Result<Capabilities> {
        // Scoping is a policy boundary; it changes no durability, atomicity, or
        // distribution property. Forwarding keeps the answer honest instead of
        // reporting a conservative all-false set for a local backend.
        self.inner.capabilities().await
    }

    async fn read(&self, path: &str) -> Result<Vec<u8>> {
        self.check_read(path)?;
        self.inner.read(path).await
    }

    async fn read_limited(&self, path: &str, max_bytes: u64) -> Result<Vec<u8>> {
        self.check_read(path)?;
        self.inner.read_limited(path, max_bytes).await
    }

    async fn write(&self, path: &str, data: &[u8]) -> Result<()> {
        self.check_write(path)?;
        self.inner.write(path, data).await
    }

    async fn append(&self, path: &str, data: &[u8]) -> Result<()> {
        self.check_write(path)?;
        self.inner.append(path, data).await
    }

    async fn rename(&self, from: &str, to: &str) -> Result<()> {
        self.check_write(from)?;
        self.check_write(to)?;
        self.inner.rename(from, to).await
    }

    async fn delete(&self, path: &str) -> Result<()> {
        self.check_write(path)?;
        self.inner.delete(path).await
    }

    async fn remove_all(&self, path: &str) -> Result<()> {
        self.check_write(path)?;
        self.inner.remove_all(path).await
    }

    async fn stat(&self, path: &str) -> Result<Stat> {
        self.check_read(path)?;
        self.inner.stat(path).await
    }

    async fn exists(&self, path: &str) -> Result<bool> {
        self.check_read(path)?;
        self.inner.exists(path).await
    }

    async fn list(&self, dir: &str) -> Result<Vec<Entry>> {
        self.check_read(dir)?;
        self.inner.list(dir).await
    }
}

/// Settings for the scoped decorator.
#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
struct Settings {
    #[serde(default)]
    deny_read: Vec<String>,
    #[serde(default)]
    allow_write: Vec<String>,
    #[serde(default)]
    mandatory_deny: Vec<String>,
}

fn validate(value: &Value) -> Result<()> {
    let settings: Settings = flowcraft_resource::settings(value)?;
    Policy::new(
        &settings.deny_read,
        &settings.allow_write,
        &settings.mandatory_deny,
    )?;
    Ok(())
}

/// The `workspace.Workspace/scoped` factory.
pub struct ScopedFactory;

impl Factory for ScopedFactory {
    type Output = Handle;

    fn spec(&self) -> Spec {
        let kind = Kind::new(KIND).unwrap_or_else(|error| panic!("{KIND} is invalid: {error}"));
        let mut workspace = require::<Handle>("workspace");
        workspace.contract = Some(CONTRACT);
        Spec {
            contract: Some(CONTRACT),
            imp: Some("scoped"),
            deps: vec![workspace],
            phase: Phase::Main,
            validate_settings: Some(validate),
            ..Spec::new(kind)
        }
    }

    fn build<'a>(&'a self, ctx: BuildCtx<'a>) -> BoxFuture<'a, Result<Arc<Self::Output>>> {
        Box::pin(async move {
            let settings: Settings = ctx.settings()?;
            let inner = ctx.deps.get::<Handle>("workspace")?;
            let policy = Policy::new(
                &settings.deny_read,
                &settings.allow_write,
                &settings.mandatory_deny,
            )?;
            Ok(Arc::new(Handle::new(Arc::new(ScopedWorkspace::new(
                Handle::new(inner.workspace()),
                policy,
            )))))
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::local::LocalWorkspace;
    use std::sync::Mutex;

    #[derive(Default)]
    struct Recorder {
        records: Mutex<Vec<ViolationRecord>>,
    }

    impl ViolationSink for Recorder {
        fn record(&self, record: &ViolationRecord) {
            self.records.lock().unwrap().push(record.clone());
        }
    }

    fn scoped(
        workspace: LocalWorkspace,
        deny_read: &[&str],
        allow_write: &[&str],
        mandatory_deny: &[&str],
    ) -> (ScopedWorkspace, Arc<Recorder>) {
        let strings = |items: &[&str]| {
            items
                .iter()
                .map(|item| (*item).to_owned())
                .collect::<Vec<_>>()
        };
        let policy = Policy::new(
            &strings(deny_read),
            &strings(allow_write),
            &strings(mandatory_deny),
        )
        .unwrap();
        let recorder = Arc::new(Recorder::default());
        let scoped = ScopedWorkspace::new(Handle::new(Arc::new(workspace)), policy)
            .with_sink(recorder.clone());
        (scoped, recorder)
    }

    async fn workspace() -> (tempfile::TempDir, LocalWorkspace) {
        let dir = tempfile::tempdir().unwrap();
        let workspace = LocalWorkspace::open(dir.path().join("root")).unwrap();
        (dir, workspace)
    }

    #[tokio::test]
    async fn deny_read_blocks_reads_only() {
        let (_dir, workspace) = workspace().await;
        workspace.write("public/a.txt", b"a").await.unwrap();
        workspace.write("private/b.txt", b"b").await.unwrap();
        let (scoped, recorder) = scoped(workspace, &["private/**"], &["**"], &[]);

        assert_eq!(scoped.read("public/a.txt").await.unwrap(), b"a");
        let error = scoped.read("private/b.txt").await.unwrap_err();
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::Forbidden);
        // The directory itself is denied too: `private/**` names `private`.
        assert!(scoped.stat("private").await.is_err());
        assert_eq!(recorder.records.lock().unwrap().len(), 2);
        assert_eq!(
            recorder.records.lock().unwrap()[0].reason,
            "deny-read pattern matched"
        );
    }

    #[tokio::test]
    async fn writes_need_an_allow_rule() {
        let (_dir, workspace) = workspace().await;
        workspace.write("existing.txt", b"x").await.unwrap();
        let (scoped, recorder) = scoped(workspace, &[], &["out/**"], &[]);

        scoped.write("out/new.txt", b"y").await.unwrap();
        assert_eq!(scoped.read("out/new.txt").await.unwrap(), b"y");
        let error = scoped.write("existing.txt", b"z").await.unwrap_err();
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::Forbidden);
        assert_eq!(
            recorder.records.lock().unwrap()[0].reason,
            "not in allow-write list"
        );
    }

    #[tokio::test]
    async fn an_empty_allow_list_makes_the_provider_read_only() {
        let (_dir, workspace) = workspace().await;
        workspace.write("file.txt", b"x").await.unwrap();
        let (scoped, _recorder) = scoped(workspace, &[], &[], &[]);
        assert_eq!(scoped.read("file.txt").await.unwrap(), b"x");
        assert!(scoped.write("file.txt", b"y").await.is_err());
        assert!(scoped.delete("file.txt").await.is_err());
    }

    #[tokio::test]
    async fn mandatory_deny_wins_over_an_allow_rule() {
        let (_dir, workspace) = workspace().await;
        let (scoped, _recorder) = scoped(workspace, &[], &["**"], &["**/.env"]);
        let error = scoped.write(".env", b"SECRET=1").await.unwrap_err();
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::Forbidden);
        assert!(error.to_string().contains("mandatory deny"));
    }

    #[tokio::test]
    async fn a_rename_checks_both_sides() {
        let (_dir, workspace) = workspace().await;
        workspace.write("in.txt", b"x").await.unwrap();
        let (scoped, _recorder) = scoped(workspace, &[], &["in.txt"], &[]);
        // Reading the destination is denied by nothing, but writing it is not
        // allowed, so the move is refused even though the source is allowed.
        assert!(scoped.rename("in.txt", "out.txt").await.is_err());
        assert!(scoped.rename("in.txt", "in.txt").await.is_ok());
    }

    #[tokio::test]
    async fn capabilities_and_bounded_reads_are_forwarded() {
        let (_dir, workspace) = workspace().await;
        workspace.write("file", b"0123456789").await.unwrap();
        let (scoped, _recorder) = scoped(workspace, &[], &["**"], &[]);
        assert!(scoped.capabilities().await.unwrap().atomic_rename);
        // A cap at or above the size succeeds; below it the read is refused
        // rather than truncated.
        assert_eq!(scoped.read_limited("file", 10).await.unwrap().len(), 10);
        assert!(scoped.read_limited("file", 9).await.is_err());
    }

    #[tokio::test]
    async fn the_wrapped_provider_is_reachable_and_unscoped() {
        let (_dir, workspace) = workspace().await;
        workspace.write("private/b.txt", b"b").await.unwrap();
        let (scoped, _recorder) = scoped(workspace, &["private/**"], &["**"], &[]);
        assert!(scoped.read("private/b.txt").await.is_err());
        // The accessor exists so provider-specific extras stay reachable; the
        // caveat is that the returned handle is not filtered.
        assert_eq!(scoped.provider().read("private/b.txt").await.unwrap(), b"b");
    }

    #[test]
    fn an_invalid_pattern_fails_validation_not_the_first_request() {
        let settings = serde_json::json!({"deny_read": ["a[b"]});
        assert_eq!(
            validate(&settings).unwrap_err().kind(),
            flowcraft_error::ErrorKind::Validation
        );
    }

    #[test]
    fn unknown_settings_fields_are_rejected() {
        let settings = serde_json::json!({"enabled": true});
        assert!(validate(&settings).is_err());
    }
}
