//! The secret-store contract.

use std::sync::Arc;

use crate::error::Result;
use crate::resource::Resource;
use crate::resource::resolver::SecretStore as ResourceSecretStore;
use async_trait::async_trait;

/// The registry kind.
pub const KIND: &str = "secret.Store";
/// The cross-process contract identity.
pub const CONTRACT: &str = "secret.Store/v1";

/// A read-only secret lookup.
#[async_trait]
pub trait Store: Send + Sync {
    /// Resolve a name. `Ok(None)` means "known namespace, absent secret";
    /// an error means the lookup itself failed.
    async fn lookup(&self, name: &str) -> Result<Option<String>>;

    /// The store's write side, when it has one.
    ///
    /// Kept as an optional view rather than a second trait bound so a provider
    /// can be read-only (a remote store) without a wrapper type, and a caller
    /// can ask "can I manage this?" without downcasting.
    fn mutable(&self) -> Option<&dyn Mutable> {
        None
    }
}

/// A store the application can write to: settings pages, CLIs, and the cleanup
/// path that runs when a plugin or a deployment goes away.
#[async_trait]
pub trait Mutable: Send + Sync {
    /// Store one secret.
    async fn set(&self, name: &str, value: &str) -> Result<()>;

    /// Remove one secret. A name that is not there is not an error.
    async fn delete(&self, name: &str) -> Result<()>;

    /// Remove every secret whose name starts with `prefix`.
    ///
    /// An empty prefix is refused: it matches every secret, and this is the
    /// cleanup hook a "the plugin or deployment is gone" path calls.
    async fn delete_prefix(&self, prefix: &str) -> Result<()>;

    /// Whether the backend can serve reads and writes right now.
    fn available(&self) -> bool;
}

/// The value a factory of this capability produces.
///
/// The metadata travels with the value because the deployment needs it after
/// construction: `id` is what `${secret:ID.NAME}` addresses, and `default`
/// marks the target of a plain `${secret:NAME}`. At most one store in a
/// deployment may be the default.
#[derive(Clone)]
pub struct Handle {
    store: Arc<dyn Store>,
    id: Option<String>,
    default: bool,
}

impl Handle {
    /// Wrap a provider.
    #[must_use]
    pub fn new(store: Arc<dyn Store>) -> Self {
        Self {
            store,
            id: None,
            default: false,
        }
    }

    /// Wrap a provider with its deployment metadata.
    #[must_use]
    pub fn with_settings(store: Arc<dyn Store>, id: Option<String>, default: bool) -> Self {
        Self {
            store,
            id: id.filter(|id| !id.is_empty()),
            default,
        }
    }

    /// The provider.
    #[must_use]
    pub fn store(&self) -> Arc<dyn Store> {
        self.store.clone()
    }

    /// The settings `id`, when one was declared.
    #[must_use]
    pub fn id(&self) -> Option<&str> {
        self.id.as_deref()
    }

    /// Whether this store is the deployment's default.
    #[must_use]
    pub fn is_default(&self) -> bool {
        self.default
    }

    /// The write side, when the provider has one.
    #[must_use]
    pub fn mutable(&self) -> Option<&dyn Mutable> {
        self.store.mutable()
    }
}

impl std::fmt::Debug for Handle {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("secret.Handle")
            .field("id", &self.id)
            .field("default", &self.default)
            .finish_non_exhaustive()
    }
}

impl std::ops::Deref for Handle {
    type Target = Arc<dyn Store>;

    fn deref(&self) -> &Self::Target {
        &self.store
    }
}

impl Resource for Handle {}

#[async_trait]
impl ResourceSecretStore for Handle {
    async fn lookup(&self, name: &str) -> Result<Option<String>> {
        self.store.lookup(name).await
    }
}
