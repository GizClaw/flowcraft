//! A secret backend: one name in, one value or a refusal out.

use crate::Result;

/// The deployment resource kind of a secret store: core's `secret.ResourceKind`.
pub const RESOURCE_KIND: &str = "secret.Store";

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

/// A built `secret.Store` resource value: a lookup backend with an optional
/// reference id and the default flag.
///
/// core's `secret.Store`; the difference from [`SecretStoreFunc`] is that this
/// one answers [`SecretStore::secret_store_id`], which the deploy builder needs
/// to decide what `${secret:ID.NAME}` matches.
pub struct Store {
    id: Option<String>,
    default: bool,
    lookup: LookupFn,
}

/// The boxed lookup a [`Store`] holds.
type LookupFn = Box<dyn Fn(&str) -> Result<Option<String>> + Send + Sync>;

impl Store {
    /// Returns a store that is not the default and has no id of its own.
    pub fn new(lookup: impl Fn(&str) -> Result<Option<String>> + Send + Sync + 'static) -> Self {
        Self {
            id: None,
            default: false,
            lookup: Box::new(lookup),
        }
    }

    /// Sets the short id `${secret:ID.NAME}` references match.
    pub fn with_id(mut self, id: impl Into<String>) -> Self {
        self.id = Some(id.into());
        self
    }

    /// Marks the store as the target of NAME-only references.
    pub fn with_default(mut self, default: bool) -> Self {
        self.default = default;
        self
    }
}

impl SecretStore for Store {
    fn lookup(&self, name: &str) -> Result<Option<String>> {
        (self.lookup)(name)
    }

    fn default_secret_store(&self) -> bool {
        self.default
    }

    fn secret_store_id(&self) -> Option<&str> {
        self.id.as_deref()
    }
}

impl std::fmt::Debug for Store {
    /// Prints the two flags and never the lookup.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Store")
            .field("id", &self.id)
            .field("default", &self.default)
            .finish_non_exhaustive()
    }
}
