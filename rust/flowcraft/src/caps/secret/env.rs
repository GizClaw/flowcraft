//! Environment provider: `prefix + name` becomes an environment variable.

use std::sync::Arc;

use crate::error::{Error, Result};
use crate::resource::Kind;
use crate::resource::{BoxFuture, BuildCtx, Factory, Phase, Spec};
use async_trait::async_trait;
use serde::Deserialize;
use serde_json::Value;

use crate::caps::secret::contract::{CONTRACT, Handle, KIND, Store};

/// Reads secrets from the process environment.
#[derive(Debug, Clone, Default)]
pub struct EnvStore {
    prefix: String,
}

impl EnvStore {
    /// Build with an optional variable-name prefix.
    #[must_use]
    pub fn new(prefix: impl Into<String>) -> Self {
        Self {
            prefix: prefix.into(),
        }
    }
}

#[async_trait]
impl Store for EnvStore {
    async fn lookup(&self, name: &str) -> Result<Option<String>> {
        if name.trim().is_empty() {
            return Err(crate::caps::secret::codes::validation(
                crate::caps::secret::codes::Code::NameInvalid,
                "secret name is empty",
            ));
        }
        if name.contains(['/', '\\', ' ', '=']) {
            return Err(Error::validation(format!(
                "secret name {name:?} cannot map to an environment variable"
            )));
        }
        Ok(std::env::var(format!("{}{name}", self.prefix)).ok())
    }
}

#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
struct Settings {
    #[serde(default)]
    prefix: String,
    #[serde(default)]
    id: Option<String>,
    #[serde(default)]
    default: bool,
}

fn validate(value: &Value) -> Result<()> {
    let settings: Settings = crate::resource::settings(value)?;
    if settings.prefix.contains(['/', '\\', ' ', '=']) {
        return Err(Error::validation(
            "settings.prefix must be usable in an environment variable name",
        ));
    }
    Ok(())
}

/// The `secret.Store/env` factory.
pub struct EnvFactory;

impl Factory for EnvFactory {
    type Output = Handle;

    fn spec(&self) -> Spec {
        let kind = Kind::new(KIND).unwrap_or_else(|error| panic!("{KIND} is invalid: {error}"));
        Spec {
            contract: Some(CONTRACT),
            imp: Some("env"),
            deps: Vec::new(),
            phase: Phase::Pre,
            validate_settings: Some(validate),
            ..Spec::new(kind)
        }
    }

    fn build<'a>(&'a self, ctx: BuildCtx<'a>) -> BoxFuture<'a, Result<Arc<Self::Output>>> {
        Box::pin(async move {
            let settings: Settings = ctx.settings()?;
            Ok(Arc::new(Handle::with_settings(
                Arc::new(EnvStore::new(settings.prefix)),
                settings.id,
                settings.default,
            )))
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn reads_an_existing_variable_through_the_prefix() {
        // `PATH` is the variable every platform this crate builds for sets;
        // `HOME` is unix spelling and Windows leaves it unset.
        let store = EnvStore::new("");
        assert!(store.lookup("PATH").await.unwrap().is_some());
        let store = EnvStore::new("FLOWCRAFT_TEST_");
        assert_eq!(store.lookup("PATH").await.unwrap(), None);
    }

    #[tokio::test]
    async fn names_that_cannot_be_variables_are_rejected() {
        let store = EnvStore::default();
        assert_eq!(
            store.lookup("a/b").await.unwrap_err().kind(),
            crate::error::ErrorKind::Validation
        );
    }
}
