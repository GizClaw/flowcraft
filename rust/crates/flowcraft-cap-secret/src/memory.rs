//! In-memory provider: tests, dev, and the base for `file`.
//!
//! It is the one provider whose contents are part of the process, so it is also
//! the one that supports writes: a test or a dev harness can set a secret after
//! the graph is built.

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use async_trait::async_trait;
use flowcraft_error::{Error, Result};
use flowcraft_resource::Kind;
use flowcraft_resource::{BoxFuture, BuildCtx, Factory, Phase, Spec};
use serde::Deserialize;
use serde_json::Value;

use crate::contract::{CONTRACT, Handle, KIND, Mutable, Store};

/// A map-backed secret store.
#[derive(Debug, Default)]
pub struct MemoryStore {
    values: Mutex<BTreeMap<String, String>>,
}

impl MemoryStore {
    /// Build from an iterator of name/value pairs.
    #[must_use]
    pub fn new(values: impl IntoIterator<Item = (String, String)>) -> Self {
        Self {
            values: Mutex::new(values.into_iter().collect()),
        }
    }
}

#[async_trait]
impl Store for MemoryStore {
    async fn lookup(&self, name: &str) -> Result<Option<String>> {
        if name.trim().is_empty() {
            return Err(Error::validation("secret name is empty"));
        }
        let values = self
            .values
            .lock()
            .map_err(|_| Error::internal("memory secret store: lock is poisoned"))?;
        Ok(values.get(name).cloned())
    }

    fn mutable(&self) -> Option<&dyn Mutable> {
        Some(self)
    }
}

#[async_trait]
impl Mutable for MemoryStore {
    async fn set(&self, name: &str, value: &str) -> Result<()> {
        if name.trim().is_empty() {
            return Err(Error::validation("secret name is empty"));
        }
        let mut values = self
            .values
            .lock()
            .map_err(|_| Error::internal("memory secret store: lock is poisoned"))?;
        values.insert(name.to_owned(), value.to_owned());
        Ok(())
    }

    async fn delete(&self, name: &str) -> Result<()> {
        let mut values = self
            .values
            .lock()
            .map_err(|_| Error::internal("memory secret store: lock is poisoned"))?;
        values.remove(name);
        Ok(())
    }

    async fn delete_prefix(&self, prefix: &str) -> Result<()> {
        if prefix.trim().is_empty() {
            return Err(Error::validation(
                "memory secret store: refusing to delete every secret with an empty prefix",
            ));
        }
        let mut values = self
            .values
            .lock()
            .map_err(|_| Error::internal("memory secret store: lock is poisoned"))?;
        values.retain(|name, _| !name.starts_with(prefix));
        Ok(())
    }

    fn available(&self) -> bool {
        true
    }
}

#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
struct Settings {
    #[serde(default)]
    values: BTreeMap<String, String>,
    #[serde(default)]
    id: Option<String>,
    #[serde(default)]
    default: bool,
}

fn validate(value: &Value) -> Result<()> {
    let settings: Settings = flowcraft_resource::settings(value)?;
    for name in settings.values.keys() {
        if name.trim().is_empty() {
            return Err(Error::validation(
                "settings.values: secret names must not be empty",
            ));
        }
    }
    Ok(())
}

/// The `secret.Store/memory` factory.
pub struct MemoryFactory;

impl Factory for MemoryFactory {
    type Output = Handle;

    fn spec(&self) -> Spec {
        let kind = Kind::new(KIND).unwrap_or_else(|error| panic!("{KIND} is invalid: {error}"));
        Spec {
            contract: Some(CONTRACT),
            imp: Some("memory"),
            deps: Vec::new(),
            phase: Phase::Pre,
            validate_settings: Some(validate),
            ..Spec::new(kind)
        }
    }

    fn build<'a>(&'a self, ctx: BuildCtx<'a>) -> BoxFuture<'a, Result<Arc<Self::Output>>> {
        Box::pin(async move {
            let settings: Settings = ctx.settings()?;
            let store = MemoryStore::new(settings.values);
            Ok(Arc::new(Handle::with_settings(
                Arc::new(store),
                settings.id,
                settings.default,
            )))
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sample() -> MemoryStore {
        MemoryStore::new([("token".to_owned(), "s3cr3t".to_owned())])
    }

    #[tokio::test]
    async fn looks_up_and_reports_absence() {
        let store = sample();
        assert_eq!(
            store.lookup("token").await.unwrap(),
            Some("s3cr3t".to_owned())
        );
        assert_eq!(store.lookup("missing").await.unwrap(), None);
    }

    #[tokio::test]
    async fn empty_names_are_rejected() {
        let store = MemoryStore::default();
        assert_eq!(
            store.lookup("  ").await.unwrap_err().kind(),
            flowcraft_error::ErrorKind::Validation
        );
    }

    #[tokio::test]
    async fn writes_are_visible_and_cleanup_is_prefixed() {
        let store = sample();
        store.set("team/a", "1").await.unwrap();
        store.set("team/b", "2").await.unwrap();
        assert_eq!(store.lookup("team/a").await.unwrap(), Some("1".into()));

        store.delete("team/a").await.unwrap();
        assert_eq!(store.lookup("team/a").await.unwrap(), None);
        // Deleting a missing name is not an error.
        store.delete("team/a").await.unwrap();

        assert!(
            store
                .delete_prefix("")
                .await
                .unwrap_err()
                .to_string()
                .contains("empty prefix")
        );
        store.delete_prefix("team/").await.unwrap();
        assert_eq!(store.lookup("team/b").await.unwrap(), None);
        // The unprefixed secret survived.
        assert_eq!(store.lookup("token").await.unwrap(), Some("s3cr3t".into()));
    }

    #[tokio::test]
    async fn the_handle_exposes_the_write_side() {
        let handle = Handle::with_settings(
            Arc::new(MemoryStore::default()),
            Some("mem".to_owned()),
            true,
        );
        let mutable = handle.mutable().expect("memory is mutable");
        mutable.set("k", "v").await.unwrap();
        assert_eq!(handle.lookup("k").await.unwrap(), Some("v".into()));
    }
}
