//! The `secret.Store` `env` implementation: one environment variable per secret.
//!
//! core's factory is `secret/envFactory`. The port builds the store directly
//! ([`EnvSettings::build`]) because there is no resource registry in the port
//! yet; the settings decode is core's, including the sentence a strict decode
//! produces.

use std::sync::Arc;

use flowcraft_utils::decode::go_quote;
use serde::Deserialize;
use serde_json::Value;

use crate::store::Store;
use crate::{Result, SecretStore, validation};

/// The settings subtree of the `env` secret store.
#[derive(Debug, Clone, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EnvSettings {
    /// The short name used in `${secret:ID.NAME}` references; `None` falls back
    /// to the resource name.
    #[serde(default, deserialize_with = "crate::go_null")]
    pub id: Option<String>,
    /// Marks this store as the target of NAME-only `${secret:NAME}` references.
    #[serde(default, deserialize_with = "crate::go_null")]
    pub default: bool,
}

impl EnvSettings {
    /// The implementation name the deployment uses for this store.
    pub const IMPL: &'static str = "env";

    /// Decodes the settings subtree strictly: an unknown field is a refusal, so
    /// a typo fails the build instead of silently dropping a policy.
    ///
    /// A `null` subtree is the empty document, which is `resource.DecodeSettings`
    /// decoding zero bytes as `{}`; nothing else is filled in.
    ///
    /// The sentence is `secret env store: decode settings: resource settings
    /// decode: resource settings: <decoder>`, which is core's wrapping with the
    /// decoder's own words last -- those belong to `encoding/json` there and to
    /// `serde` here, and they are not compared.
    pub fn decode(settings: &Value) -> Result<Self> {
        let settings = if settings.is_null() {
            Value::Object(serde_json::Map::new())
        } else {
            settings.clone()
        };
        serde_json::from_value(settings).map_err(|err| {
            validation(format!(
                "secret env store: decode settings: resource settings decode: \
                 resource settings: {err}"
            ))
        })
    }

    /// Builds the store.
    ///
    /// A variable that is set but empty is *found* (with an empty value), which
    /// is `os.LookupEnv`'s answer; a variable that does not exist is not-found
    /// rather than a failure.
    pub fn build(&self) -> Arc<dyn SecretStore> {
        let mut store = Store::new(|name: &str| -> Result<Option<String>> {
            match std::env::var(name) {
                Ok(value) => Ok(Some(value)),
                Err(std::env::VarError::NotPresent) => Ok(None),
                // core carries the raw bytes and never asks whether they are
                // UTF-8; a port that answered "not found" here would refuse a
                // secret silently, and one that mangled it would hand back the
                // wrong key. So it is a refusal, and a port-only one.
                Err(std::env::VarError::NotUnicode(_)) => Err(validation(format!(
                    "secret env store: variable {} is not valid UTF-8",
                    go_quote(name)
                ))),
            }
        })
        .with_default(self.default);
        if let Some(id) = &self.id {
            store = store.with_id(id.clone());
        }
        Arc::new(store)
    }
}
