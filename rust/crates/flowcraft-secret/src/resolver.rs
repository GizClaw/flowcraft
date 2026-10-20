//! The `${secret:...}` scheme, and the resolver that materializes a reference.

use std::collections::BTreeMap;
use std::sync::Arc;

use flowcraft_utils::decode::go_quote;
use serde::{Deserialize, Serialize};

use crate::store::SecretStore;
use crate::{Result, validation};

/// The wire shape a `${secret:...}` span expands to: which store, which name.
///
/// It is a *marker*, not a value. core's scheme returns it as the substituted
/// settings value, the settings tree then decodes it into
/// [`Secret`](crate::Secret), and the actual lookup waits until somebody asks
/// for the value.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct SecretRef {
    /// The store id the reference named.
    pub store: String,
    /// The secret name inside that store.
    pub name: String,
}

/// Maps store ids to [`SecretStore`]s and resolves `${secret:...}` references.
///
/// The deploy builder assembles one from the declared `secret.Store` resources
/// and hands it to factories, so a typed [`Secret`](crate::Secret) resolves
/// lazily at use time.
#[derive(Clone)]
pub struct SecretResolver {
    stores: BTreeMap<String, Arc<dyn SecretStore>>,
    default_store: String,
}

impl std::fmt::Debug for SecretResolver {
    /// Prints the store ids and the default, never the stores: a store holds
    /// secrets, and `dyn SecretStore` has no `Debug` to promise otherwise.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("SecretResolver")
            .field("stores", &self.stores.keys().collect::<Vec<_>>())
            .field("default_store", &self.default_store)
            .finish()
    }
}

impl SecretResolver {
    /// Returns a resolver over `stores`. `default_store` is the target of
    /// NAME-only references; empty means NAME-only references are refused.
    pub fn new(stores: BTreeMap<String, Arc<dyn SecretStore>>, default_store: String) -> Self {
        Self {
            stores,
            default_store,
        }
    }

    /// Returns the store registered under `id`, if any.
    pub fn store(&self, id: &str) -> Option<&Arc<dyn SecretStore>> {
        self.stores.get(id)
    }

    /// Expands one reference path -- the text after `${secret:` -- into the
    /// marker it stands for.
    ///
    /// `NAME` resolves through the default store and `store.NAME` through the
    /// named one. A dotted path whose prefix names no configured store is the
    /// whole secret name in the default store, so dotted names (docker/k8s file
    /// secrets like `my.app.key`) stay addressable.
    ///
    /// This is build time: the store has to exist and the name has to be
    /// non-blank, but no value is read.
    pub fn expand(&self, path: &str) -> Result<SecretRef> {
        let (store, name) = self.split(path);
        if store.is_empty() {
            return Err(validation(format!(
                "resource settings expand: secret reference ${{secret:{path}}} \
                 needs an explicit store or a default secret.Store"
            )));
        }
        if !self.stores.contains_key(&store) {
            return Err(validation(format!(
                "resource settings expand: secret store {} is not configured",
                go_quote(&store)
            )));
        }
        let name = name.trim();
        if name.is_empty() {
            return Err(validation("resource settings expand: secret name is empty"));
        }
        Ok(SecretRef {
            store,
            name: name.to_string(),
        })
    }

    /// Materializes one secret through its store.
    ///
    /// A backend failure stays a validation error whose sentence contains the
    /// store's own: core wraps it rather than reclassifying it, and a caller that
    /// needs to tell "no such secret" from "the store is down" has to read the
    /// kind the store returned, which this signature discards. That is core's
    /// behaviour, not a simplification.
    pub fn resolve(&self, store: &str, name: &str) -> Result<String> {
        let Some(backend) = self.stores.get(store) else {
            return Err(validation(format!(
                "resource secret: store {} is not configured",
                go_quote(store)
            )));
        };
        let value = backend.lookup(name).map_err(|err| {
            validation(format!("resource secret: store {}: {err}", go_quote(store)))
        })?;
        value.ok_or_else(|| {
            validation(format!(
                "resource secret: {} not found in store {}",
                go_quote(name),
                go_quote(store)
            ))
        })
    }

    /// Splits `store.NAME` from `NAME`, core's `resolveRef`.
    ///
    /// A dotted ref whose prefix names a configured store is an explicit
    /// `store.NAME` reference; otherwise the whole ref is the secret name in the
    /// default store. The prefix is compared as written -- core does not trim it
    /// -- and the name keeps its space until [`SecretResolver::expand`] trims it.
    fn split(&self, path: &str) -> (String, String) {
        if let Some((before, after)) = path.split_once('.')
            && !before.trim().is_empty()
            && self.stores.contains_key(before)
        {
            return (before.to_string(), after.to_string());
        }
        (self.default_store.clone(), path.to_string())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use flowcraft_contract::error::ErrorKind;

    #[test]
    fn every_refusal_is_a_validation_error() {
        let empty = SecretResolver::new(BTreeMap::new(), String::new());
        let refusal = empty.expand("TOKEN").err().unwrap_or_else(|| {
            panic!("an empty resolver expanded a reference");
        });
        assert_eq!(refusal.kind(), ErrorKind::Validation);
    }
}
