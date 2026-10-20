//! The `secret.Store` `file` implementation: one file per secret under a base
//! directory.
//!
//! This mirrors deployment conventions (docker secrets, mounted k8s secrets):
//! the secret's name is a file name, its value is the file's contents, and the
//! newline `echo` adds is not part of the key.
//!
//! core's factory is `secret/fileFactory`. The port builds the store directly
//! ([`FileSettings::build`]) because there is no resource registry yet.

use std::sync::Arc;

use serde::Deserialize;
use serde_json::Value;

use crate::loader::Loader;
use crate::store::Store;
use crate::{Result, SecretStore, validation};

/// The settings subtree of the `file` secret store.
#[derive(Debug, Clone, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct FileSettings {
    /// The short name used in `${secret:ID.NAME}` references; `None` falls back
    /// to the resource name.
    #[serde(default, deserialize_with = "crate::go_null")]
    pub id: Option<String>,
    /// Marks this store as the target of NAME-only `${secret:NAME}` references.
    #[serde(default, deserialize_with = "crate::go_null")]
    pub default: bool,
    /// The directory secret files live in. Required. Lookups cannot escape it
    /// (lexically or through symlinks) and are capped at the loader's size
    /// limit.
    #[serde(default, deserialize_with = "crate::go_null")]
    pub base: String,
}

impl FileSettings {
    /// The implementation name the deployment uses for this store.
    pub const IMPL: &'static str = "file";

    /// Decodes the settings subtree strictly, then requires a base directory.
    ///
    /// The blank check runs *after* the decode, as core's does: a `base` of
    /// spaces decodes fine and is refused as missing, and the sentence names the
    /// field rather than repeating the value.
    pub fn decode(settings: &Value) -> Result<Self> {
        let settings = if settings.is_null() {
            Value::Object(serde_json::Map::new())
        } else {
            settings.clone()
        };
        let decoded: Self = serde_json::from_value(settings).map_err(|err| {
            validation(format!(
                "secret file store: decode settings: resource settings decode: \
                 resource settings: {err}"
            ))
        })?;
        if decoded.base.trim().is_empty() {
            return Err(validation("secret file store: settings.base is required"));
        }
        Ok(decoded)
    }

    /// Builds the store: one loader over [`FileSettings::base`], one file per
    /// lookup.
    ///
    /// A name that is not there is *not-found*, which the resolver turns into
    /// its own refusal; every other loader refusal (a path that escapes, a file
    /// over the cap, an unreadable base directory) is passed through unchanged.
    /// core does the same, so the sentence a caller sees names the loader, not
    /// the secret store.
    ///
    /// A file whose bytes are not UTF-8 is a refusal, and that one is the port's
    /// own: core hands the bytes back as a Go string, which may hold arbitrary
    /// bytes, and [`String`] cannot. Refusing says so; a lossy conversion would
    /// hand back a different key than the one stored.
    pub fn build(&self) -> Arc<dyn SecretStore> {
        let loader = Loader::new(&self.base);
        let mut store = Store::new(move |name: &str| -> Result<Option<String>> {
            let raw = match loader.load_file(name) {
                Ok(raw) => raw,
                Err(err) if err.kind() == flowcraft_contract::error::ErrorKind::NotFound => {
                    return Ok(None);
                }
                Err(err) => return Err(err),
            };
            // docker/k8s conventions: strip the trailing newlines `echo` or
            // `printf` leave, so a key stored as a line works. core's
            // `strings.TrimRight(raw, "\r\n")` strips *every* trailing CR and
            // LF, not one.
            let text = String::from_utf8(raw).map_err(|_| {
                validation(format!(
                    "secret file store: secret {} is not valid UTF-8",
                    flowcraft_utils::decode::go_quote(name)
                ))
            })?;
            Ok(Some(text.trim_end_matches(['\r', '\n']).to_string()))
        })
        .with_default(self.default);
        if let Some(id) = &self.id {
            store = store.with_id(id.clone());
        }
        Arc::new(store)
    }
}
