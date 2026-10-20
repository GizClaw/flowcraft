//! A secret backend: one name in, one value or a refusal out.

use crate::Result;

/// Resolves one secret value by name.
///
/// Implementations are pluggable backends (environment, file, keychain, vault,
/// ...), and a deployment's `secret.Store` resources are the declarative wiring
/// around them.
pub trait SecretStore: Send + Sync {
    /// Returns the secret named `name`.
    ///
    /// `Ok(None)` means the store has no such secret; `Err` is a store failure.
    /// core spells this `(value string, found bool, err error)`, where the first
    /// two can disagree.
    fn lookup(&self, name: &str) -> Result<Option<String>>;

    /// Reports whether NAME-less `${secret:...}` references may resolve through
    /// this store. At most one store in a deployment may answer `true`.
    fn default_secret_store(&self) -> bool {
        false
    }

    /// Reports the shorter reference id the store is addressable by, if it has
    /// one; `None` means the deployment resource's name is used.
    fn secret_store_id(&self) -> Option<&str> {
        None
    }
}

/// Adapts a lookup function to [`SecretStore`], which is how a scenario builds a
/// store out of a canned answer table -- core's `resource.SecretStoreFunc`.
pub struct SecretStoreFunc<F> {
    lookup: F,
    default: bool,
}

impl<F> SecretStoreFunc<F> {
    /// Wraps `lookup` as a store that is not the default.
    pub fn new(lookup: F) -> Self {
        Self {
            lookup,
            default: false,
        }
    }

    /// Marks the store as the target of NAME-only references.
    pub fn with_default(self, default: bool) -> Self {
        Self { default, ..self }
    }
}

impl<F> SecretStore for SecretStoreFunc<F>
where
    F: Fn(&str) -> Result<Option<String>> + Send + Sync,
{
    fn lookup(&self, name: &str) -> Result<Option<String>> {
        (self.lookup)(name)
    }

    fn default_secret_store(&self) -> bool {
        self.default
    }
}

impl<F> std::fmt::Debug for SecretStoreFunc<F> {
    /// Never prints the lookup: a store holds secrets.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("SecretStoreFunc")
    }
}
