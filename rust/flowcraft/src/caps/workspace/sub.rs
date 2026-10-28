//! A prefixed view: one subtree presented as a whole workspace.
//!
//! Every operation forwards to the parent after prefixing the caller's path, so
//! a caller holding the view cannot name anything outside it: the provider's own
//! confinement applies to the joined path. That is the whole mechanism — the
//! view adds a name mapping, not a second permission system (a policy belongs in
//! [`crate::caps::workspace::scoped`]).
//!
//! Order with a policy matters for the *spelling* of its rules, not for whether
//! they hold: a policy below the view sees the prefixed path (`run-1/secrets/x`),
//! so a rule anchored at `secrets/` never matches there. Spell such a rule
//! `**/secrets/**` to make it order-independent; a test pins this
//! ([`crate::caps::workspace::scoped`]'s test module and `tests/decorators.rs`).
//!
//! A view is also how a deployment gives each run its own "root" without
//! minting a new grant or opening a second handle: `sub(prefix = "runs/42")`
//! over one provider costs a string join per call.
//!
//! One difference from the Go side's `Sub` is deliberate: the view does not
//! materialize the prefix directory. The contract has no `mkdir`, a write or a
//! rename creates the parents it needs, and a directory that does not exist yet
//! already lists as empty — so creating it at build time would only add an
//! I/O side effect to a name mapping. Go's `Sub` creates it because `Root()`
//! hands a real path to embedded stores; a Rust caller takes `root()` from the
//! provider instead.

use std::sync::Arc;

use crate::error::Result;
use crate::resource::Kind;
use crate::resource::{BoxFuture, BuildCtx, Factory, Phase, Spec, require};
use async_trait::async_trait;
use serde::Deserialize;
use serde_json::Value;

use crate::caps::workspace::codes::{self, Code};
use crate::caps::workspace::contract::{
    CONTRACT, Capabilities, Entry, Handle, KIND, Stat, Workspace,
};
use crate::caps::workspace::path;

/// A workspace view rooted under `prefix`.
#[derive(Debug)]
pub struct SubWorkspace {
    inner: Handle,
    prefix: String,
}

impl SubWorkspace {
    /// Build a view rooted at `prefix` (cleaned; `""` and `"."` mean no prefix).
    ///
    /// # Errors
    ///
    /// [`crate::error::ErrorKind::Forbidden`] when the prefix is absolute or
    /// escapes the parent root.
    pub fn new(inner: Handle, prefix: &str) -> Result<Self> {
        Ok(Self {
            inner,
            prefix: path::clean(prefix)?,
        })
    }

    /// The cleaned prefix this view forwards under.
    #[must_use]
    pub fn prefix(&self) -> &str {
        &self.prefix
    }

    /// The workspace this view prefixes.
    ///
    /// The names a caller writes through the view are relative to the prefix;
    /// the handle returned here speaks the parent's names, so both spellings of
    /// one file are reachable. It exists for the same reason the Go side lets a
    /// `ScopedWorkspace` be unwrapped: a caller that needs the underlying
    /// provider (its root path, another policy) should not have to reconstruct
    /// it.
    #[must_use]
    pub fn parent(&self) -> Handle {
        self.inner.clone()
    }

    fn join(&self, path: &str) -> Result<String> {
        Ok(path::join(&self.prefix, &path::clean(path)?))
    }
}

#[async_trait]
impl Workspace for SubWorkspace {
    async fn capabilities(&self) -> Result<Capabilities> {
        // A prefix changes names, not storage properties.
        self.inner.capabilities().await
    }

    async fn read(&self, path: &str) -> Result<Vec<u8>> {
        self.inner.read(&self.join(path)?).await
    }

    async fn read_limited(&self, path: &str, max_bytes: u64) -> Result<Vec<u8>> {
        self.inner.read_limited(&self.join(path)?, max_bytes).await
    }

    async fn write(&self, path: &str, data: &[u8]) -> Result<()> {
        self.inner.write(&self.join(path)?, data).await
    }

    async fn append(&self, path: &str, data: &[u8]) -> Result<()> {
        self.inner.append(&self.join(path)?, data).await
    }

    async fn rename(&self, from: &str, to: &str) -> Result<()> {
        self.inner.rename(&self.join(from)?, &self.join(to)?).await
    }

    async fn delete(&self, path: &str) -> Result<()> {
        self.inner.delete(&self.join(path)?).await
    }

    async fn remove_all(&self, path: &str) -> Result<()> {
        // The view's own root is off limits, exactly like a provider's root:
        // forwarding "." would ask the parent to remove the prefix itself.
        if path::clean(path)?.is_empty() {
            return Err(codes::forbidden(
                Code::RootRemoval,
                "workspace: refusing to remove root",
            ));
        }
        self.inner.remove_all(&self.join(path)?).await
    }

    async fn stat(&self, path: &str) -> Result<Stat> {
        self.inner.stat(&self.join(path)?).await
    }

    async fn exists(&self, path: &str) -> Result<bool> {
        self.inner.exists(&self.join(path)?).await
    }

    async fn list(&self, dir: &str) -> Result<Vec<Entry>> {
        self.inner.list(&self.join(dir)?).await
    }
}

/// Settings for the prefixed view.
#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct Settings {
    #[serde(default)]
    prefix: String,
}

fn validate(value: &Value) -> Result<()> {
    let settings: Settings = crate::resource::settings(value)?;
    path::clean(&settings.prefix)?;
    Ok(())
}

/// The `workspace.Workspace/sub` factory.
pub struct SubFactory;

impl Factory for SubFactory {
    type Output = Handle;

    fn spec(&self) -> Spec {
        let kind = Kind::new(KIND).unwrap_or_else(|error| panic!("{KIND} is invalid: {error}"));
        let mut workspace = require::<Handle>("workspace");
        workspace.contract = Some(CONTRACT);
        Spec {
            contract: Some(CONTRACT),
            imp: Some("sub"),
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
            let handle = Handle::new(inner.workspace());
            // No prefix means the same view: return the parent handle instead of
            // wrapping it, so a document that forgets the setting costs nothing.
            if path::clean(&settings.prefix)?.is_empty() {
                return Ok(Arc::new(handle));
            }
            Ok(Arc::new(Handle::new(Arc::new(SubWorkspace::new(
                handle,
                &settings.prefix,
            )?))))
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::caps::workspace::local::LocalWorkspace;

    async fn view(prefix: &str) -> (tempfile::TempDir, SubWorkspace, Handle) {
        let dir = tempfile::tempdir().unwrap();
        let local = LocalWorkspace::open(dir.path().join("root")).unwrap();
        let inner = Handle::new(Arc::new(local));
        let view = SubWorkspace::new(inner.clone(), prefix).unwrap();
        (dir, view, inner)
    }

    #[tokio::test]
    async fn the_view_cannot_name_anything_outside_its_prefix() {
        let (_dir, view, inner) = view("run-1").await;
        inner.write("outside.txt", b"out").await.unwrap();
        view.write("inside.txt", b"in").await.unwrap();
        assert_eq!(inner.read("run-1/inside.txt").await.unwrap(), b"in");
        assert_eq!(view.read("inside.txt").await.unwrap(), b"in");
        assert_eq!(
            view.read("../outside.txt").await.unwrap_err().kind(),
            crate::error::ErrorKind::Forbidden
        );
        // The parent's own file is invisible through the view, not merely
        // unwritable.
        assert!(!view.exists("../outside.txt").await.unwrap_or(false));
        assert_eq!(view.list("").await.unwrap().len(), 1);
    }

    #[tokio::test]
    async fn the_root_of_the_view_is_the_prefix() {
        let (_dir, view, _inner) = view("a/b").await;
        assert_eq!(view.prefix(), "a/b");
        // A prefix that nothing has written to yet is simply empty.
        assert!(view.list("").await.unwrap().is_empty());
        assert_eq!(
            view.stat(".").await.unwrap_err().kind(),
            crate::error::ErrorKind::NotFound
        );
        view.write("file", b"x").await.unwrap();
        assert_eq!(view.stat(".").await.unwrap().name, "b");
    }

    #[tokio::test]
    async fn removing_the_view_root_is_forbidden_but_a_subtree_is_not() {
        let (_dir, view, _inner) = view("run-1").await;
        view.write("nested/file", b"x").await.unwrap();
        assert_eq!(
            view.remove_all(".").await.unwrap_err().kind(),
            crate::error::ErrorKind::Forbidden
        );
        view.remove_all("nested").await.unwrap();
        assert!(!view.exists("nested").await.unwrap());
    }

    #[tokio::test]
    async fn bounded_reads_are_forwarded() {
        let (_dir, view, _inner) = view("run-1").await;
        view.write("file", b"0123456789").await.unwrap();
        assert_eq!(view.read_limited("file", 10).await.unwrap().len(), 10);
        assert!(view.read_limited("file", 9).await.is_err());
    }

    #[tokio::test]
    async fn an_escaping_prefix_is_rejected() {
        let (_dir, _view, inner) = view("run-1").await;
        for prefix in ["..", "a/../../b", "/abs"] {
            assert_eq!(
                SubWorkspace::new(inner.clone(), prefix).unwrap_err().kind(),
                crate::error::ErrorKind::Forbidden,
                "{prefix}"
            );
        }
    }

    #[tokio::test]
    async fn the_parent_is_reachable_under_its_own_names() {
        let (_dir, view, inner) = view("run-1").await;
        view.write("file", b"x").await.unwrap();
        // One file, two spellings: the view's and the parent's.
        assert_eq!(inner.read("run-1/file").await.unwrap(), b"x");
        assert_eq!(view.parent().read("run-1/file").await.unwrap(), b"x");
        assert_eq!(
            view.parent().read("file").await.unwrap_err().kind(),
            crate::error::ErrorKind::NotFound
        );
    }

    #[test]
    fn settings_validate_the_prefix() {
        assert!(validate(&serde_json::json!({"prefix": "run-1"})).is_ok());
        assert!(validate(&serde_json::json!({"prefix": "../escape"})).is_err());
        assert!(validate(&serde_json::json!({"unknown": 1})).is_err());
    }
}
