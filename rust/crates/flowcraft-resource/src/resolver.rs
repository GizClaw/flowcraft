//! `${...}` reference expansion.
//!
//! Two schemes: `${env:NAME}` and `${secret:NAME}` / `${secret:ID.NAME}`.
//! Environment references expand to text. Secret references expand to a
//! [`crate::secrets::SecretRef`] marker, which a settings field typed as
//! [`crate::secrets::Secret`] decodes; the value is looked up later, through the
//! [`crate::secrets::SecretTable`] the deployment assembled.
//!
//! A secret reference must be the *entire* settings value. Expanding one into
//! part of a longer string would have to inline the plaintext, which is the one
//! thing this layer exists to prevent.

use std::sync::Arc;

use async_trait::async_trait;
use flowcraft_error::{Error, Result};

use crate::secrets::{SecretRef, SecretTable};
use crate::spec::BoxFuture;
use serde_json::Value;

/// Host-provided secret lookup used by `${secret:...}`.
///
/// The deployment uses [`SecretTable`], which resolves by store id and caches;
/// this trait is the shape a single store speaks, and the shortcut
/// [`Resolver::with_secrets`] wraps one into a table.
#[async_trait]
pub trait SecretStore: Send + Sync {
    /// Resolve a name. `Ok(None)` means "known but absent".
    async fn lookup(&self, name: &str) -> Result<Option<String>>;
}

/// Expands `${env:...}` and `${secret:...}` references in a settings subtree.
#[derive(Default)]
pub struct Resolver {
    /// Whether `${env:...}` is allowed.
    pub env: bool,
    table: Option<Arc<SecretTable>>,
}

impl std::fmt::Debug for Resolver {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Resolver")
            .field("env", &self.env)
            .field("secrets", &self.table.is_some())
            .finish()
    }
}

/// One piece of an expanded string.
enum Segment {
    Text(String),
    Marker(SecretRef),
}

impl Resolver {
    /// A resolver that reads the process environment and no secrets.
    #[must_use]
    pub fn new() -> Self {
        Self {
            env: true,
            table: None,
        }
    }

    /// Install a single secret store, which becomes the default.
    #[must_use]
    pub fn with_secrets(self, store: Arc<dyn SecretStore>) -> Self {
        self.with_table(Arc::new(SecretTable::single(store)))
    }

    /// Install the deployment's secret table.
    #[must_use]
    pub fn with_table(mut self, table: Arc<SecretTable>) -> Self {
        self.table = Some(table);
        self
    }

    /// The installed table, when one is.
    #[must_use]
    pub fn table(&self) -> Option<&Arc<SecretTable>> {
        self.table.as_ref()
    }

    /// Expand every reference in `value`.
    pub fn expand<'a>(&'a self, value: &'a Value) -> BoxFuture<'a, Result<Value>> {
        Box::pin(async move { self.expand_value(value).await })
    }

    fn expand_value<'a>(&'a self, value: &'a Value) -> BoxFuture<'a, Result<Value>> {
        Box::pin(async move {
            match value {
                Value::String(_) => self.expand_string(value).await,
                Value::Array(items) => {
                    let mut out = Vec::with_capacity(items.len());
                    for item in items {
                        out.push(self.expand_value(item).await?);
                    }
                    Ok(Value::Array(out))
                }
                Value::Object(map) => {
                    let mut out = serde_json::Map::with_capacity(map.len());
                    for (key, item) in map {
                        out.insert(key.clone(), self.expand_value(item).await?);
                    }
                    Ok(Value::Object(out))
                }
                other => Ok(other.clone()),
            }
        })
    }

    /// Expand one string. A string that is exactly one secret reference becomes
    /// a marker object; everything else becomes text.
    fn expand_string<'a>(&'a self, value: &'a Value) -> BoxFuture<'a, Result<Value>> {
        Box::pin(async move {
            let Some(text) = value.as_str() else {
                return Ok(value.clone());
            };
            let mut segments = Vec::new();
            let mut cursor = 0usize;
            while let Some(start) = text[cursor..].find("${") {
                let start = cursor + start;
                if start > cursor {
                    segments.push(Segment::Text(text[cursor..start].to_owned()));
                }
                let after = &text[start + 2..];
                let end = after.find('}').ok_or_else(|| {
                    Error::validation(format!("unterminated reference in {text:?}"))
                })?;
                let reference = &after[..end];
                segments.push(self.resolve_reference(reference)?);
                cursor = start + 2 + end + 1;
            }
            if cursor < text.len() {
                segments.push(Segment::Text(text[cursor..].to_owned()));
            }

            if let [Segment::Marker(reference)] = segments.as_slice() {
                return marker(reference);
            }
            let mut out = String::with_capacity(text.len());
            for segment in &segments {
                match segment {
                    Segment::Text(text) => out.push_str(text),
                    Segment::Marker(reference) => {
                        return Err(Error::validation(format!(
                            "secret reference for {:?} must be the entire value, not part of {text:?}: inlining a secret would defeat lazy resolution",
                            reference_text(reference)
                        )));
                    }
                }
            }
            if segments.is_empty() {
                return Ok(Value::String(text.to_owned()));
            }
            Ok(Value::String(out))
        })
    }

    /// Classify one `${...}` token: environment references resolve to text,
    /// secret references resolve to a marker.
    fn resolve_reference(&self, reference: &str) -> Result<Segment> {
        match reference.split_once(':') {
            Some(("env", name)) => {
                if !self.env {
                    return Err(Error::forbidden(format!(
                        "environment reference {name:?} is disabled"
                    )));
                }
                let value = std::env::var(name).map_err(|_| {
                    Error::not_found(format!("environment variable {name:?} is not set"))
                })?;
                Ok(Segment::Text(value))
            }
            Some(("secret", path)) => {
                let table = self.table.as_ref().ok_or_else(|| {
                    Error::not_available(format!("secret reference {path:?} needs a secret store"))
                })?;
                if let Some(error) = table.failure() {
                    return Err(error);
                }
                let (store, name) = split_reference(table, path)?;
                Ok(Segment::Marker(SecretRef::new(store, name)))
            }
            Some((other, _)) => Err(Error::validation(format!(
                "unknown reference scheme {other:?} in ${{{reference}}}"
            ))),
            None => Err(Error::validation(format!(
                "reference {reference:?} must be scheme:name, e.g. env:NAME"
            ))),
        }
    }
}

/// Split `${secret:...}` into store id and name.
///
/// `ID.NAME` is an explicit store when `ID` names one; otherwise the whole path
/// is a name in the default store, so dotted names (`my.app.key`, a file secret)
/// stay addressable.
fn split_reference(table: &SecretTable, path: &str) -> Result<(String, String)> {
    let explicit = path.split_once('.').filter(|(id, _)| table.knows(id));
    let (store, name) = match explicit {
        Some((id, name)) => (id.to_owned(), name.to_owned()),
        None => (
            table
                .default_id()
                .ok_or_else(|| Error::validation(no_default(path)))?,
            path.to_owned(),
        ),
    };
    if name.trim().is_empty() {
        return Err(Error::validation(format!(
            "secret reference {path:?} has an empty name"
        )));
    }
    if !table.knows(&store) {
        return Err(Error::validation(format!(
            "secret reference {path:?}: store {store:?} is not configured (known: {:?})",
            table.ids()
        )));
    }
    Ok((store, name))
}

fn no_default(path: &str) -> String {
    format!("secret reference {path:?} needs an explicit store or a default secret.Store")
}

fn marker(reference: &SecretRef) -> Result<Value> {
    Ok(serde_json::json!({
        crate::secrets::MARKER: {
            "store": reference.store,
            "name": reference.name,
        }
    }))
}

fn reference_text(reference: &SecretRef) -> String {
    format!("{}.{}", reference.store, reference.name)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::secrets::Secret;
    use std::collections::BTreeMap;

    struct MapStore(BTreeMap<String, String>);

    impl MapStore {
        fn new(pairs: impl IntoIterator<Item = (&'static str, &'static str)>) -> Arc<Self> {
            Arc::new(Self(
                pairs
                    .into_iter()
                    .map(|(name, value)| (name.to_owned(), value.to_owned()))
                    .collect(),
            ))
        }
    }

    #[async_trait]
    impl SecretStore for MapStore {
        async fn lookup(&self, name: &str) -> Result<Option<String>> {
            Ok(self.0.get(name).cloned())
        }
    }

    #[tokio::test]
    async fn expands_references_inside_strings() {
        let resolver = Resolver::new();
        let value = serde_json::json!({"root": "data", "path": "x-${env:HOME}"});
        // HOME is set in every test environment; assert the shape, not the value.
        let expanded = resolver.expand(&value).await.unwrap();
        assert!(expanded["path"].as_str().unwrap().starts_with("x-/"));
        assert_eq!(expanded["root"], "data");
    }

    #[tokio::test]
    async fn missing_reference_is_not_found() {
        let resolver = Resolver::new();
        let value = serde_json::json!("${env:FLOWCRAFT_TEST_DOES_NOT_EXIST}");
        let error = resolver.expand(&value).await.unwrap_err();
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::NotFound);
    }

    #[tokio::test]
    async fn secrets_need_a_store() {
        let resolver = Resolver::new();
        let value = serde_json::json!("${secret:token}");
        let error = resolver.expand(&value).await.unwrap_err();
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::NotAvailable);
    }

    #[tokio::test]
    async fn secrets_expand_to_a_lazy_marker() {
        let resolver = Resolver::new().with_secrets(MapStore::new([("token", "s3cr3t")]));
        let value = serde_json::json!({"auth": "${secret:token}"});
        let expanded = resolver.expand(&value).await.unwrap();
        let secret: Secret = serde_json::from_value(expanded["auth"].clone()).unwrap();
        assert!(secret.is_ref());
        // The plaintext is nowhere in the expanded settings.
        assert!(!expanded.to_string().contains("s3cr3t"));
        let resolved = secret
            .resolve(resolver.table().map(|table| &**table))
            .await
            .unwrap();
        assert_eq!(resolved, "s3cr3t");
    }

    #[tokio::test]
    async fn an_explicit_store_id_selects_that_store() {
        let table = Arc::new(SecretTable::new());
        table
            .attach("first", Some("a"), true, MapStore::new([("k", "one")]))
            .unwrap();
        table
            .attach("second", Some("b"), false, MapStore::new([("k", "two")]))
            .unwrap();
        let resolver = Resolver::new().with_table(table.clone());

        let explicit = resolver
            .expand(&serde_json::json!("${secret:b.k}"))
            .await
            .unwrap();
        let secret: Secret = serde_json::from_value(explicit).unwrap();
        assert_eq!(
            secret.resolve(Some(&table)).await.unwrap(),
            "two",
            "ID.NAME must select the named store, not the default"
        );

        // A dotted name whose prefix is not a store stays a name in the default
        // store, so `my.app.key` keeps working.
        let dotted = resolver
            .expand(&serde_json::json!("${secret:my.app.key}"))
            .await
            .unwrap();
        let secret: Secret = serde_json::from_value(dotted).unwrap();
        assert_eq!(secret.as_ref().unwrap().store, "a");
        assert_eq!(secret.as_ref().unwrap().name, "my.app.key");
    }

    #[tokio::test]
    async fn a_reference_without_a_usable_store_fails_at_build_time() {
        let table = Arc::new(SecretTable::new());
        table
            .attach("only", Some("team"), false, MapStore::new([]))
            .unwrap();
        let resolver = Resolver::new().with_table(table);
        // `other` is not a store id, so the whole path is a name in the default
        // store — and this table has none.
        let error = resolver
            .expand(&serde_json::json!("${secret:other.k}"))
            .await
            .unwrap_err();
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::Validation);
    }

    #[tokio::test]
    async fn a_name_only_reference_without_a_default_store_fails() {
        let table = Arc::new(SecretTable::new());
        table
            .attach("only", Some("team"), false, MapStore::new([]))
            .unwrap();
        let resolver = Resolver::new().with_table(table);
        let error = resolver
            .expand(&serde_json::json!("${secret:token}"))
            .await
            .unwrap_err();
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::Validation);
        assert!(error.to_string().contains("default"), "{error}");
    }

    #[tokio::test]
    async fn an_embedded_secret_reference_is_rejected() {
        let resolver = Resolver::new().with_secrets(MapStore::new([("token", "s3cr3t")]));
        let error = resolver
            .expand(&serde_json::json!("Bearer ${secret:token}"))
            .await
            .unwrap_err();
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::Validation);
        assert!(error.to_string().contains("entire value"), "{error}");
        assert!(!error.to_string().contains("s3cr3t"), "{error}");
    }

    #[tokio::test]
    async fn a_recorded_attach_failure_stops_expansion() {
        let table = Arc::new(SecretTable::new());
        table
            .attach("a", None, true, MapStore::new([("k", "1")]))
            .unwrap();
        assert!(
            table
                .attach("b", None, true, MapStore::new([("k", "2")]))
                .is_err()
        );
        let resolver = Resolver::new().with_table(table);
        let error = resolver
            .expand(&serde_json::json!("${secret:k}"))
            .await
            .unwrap_err();
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::Conflict);
    }
}
