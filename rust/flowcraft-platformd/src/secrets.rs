//! Deployment secrets: the store table, the build observer, and host injection.
//!
//! `${secret:...}` is resolved while the graph is built, but the store that
//! answers it is itself a resource. The kernel solves the ordering with
//! `Phase::Pre`; this module closes the loop: every built `secret.Store` is
//! attached to one [`SecretTable`] with its `id`/`default` metadata, and the
//! resolver consults that table for everything built afterwards.
//!
//! Two rules ride along with the metadata, both from the deployment contract:
//!
//! * at most one store may declare `default: true` (a second one is a conflict,
//!   reported rather than silently preferred);
//! * two stores may not share an `id`.
//!
//! A process that owns a secret store of its own (a settings page, a keychain
//! the host opened) can inject it with [`crate::assembly::BuildOptions::with_default_secret`]. It
//! joins the same table, so the document does not need to declare it and the
//! graph does not need to build it.

use std::sync::Arc;

use flowcraft::caps::secret::Handle as SecretHandle;
use flowcraft::error::Result;
use flowcraft::resource::resolver::SecretStore;
use flowcraft::resource::{BuildObserver, SecretTable, Value};

/// The table plus the observer that fills it.
#[derive(Clone)]
pub struct DeploymentSecrets {
    table: Arc<SecretTable>,
}

impl Default for DeploymentSecrets {
    fn default() -> Self {
        Self::new()
    }
}

impl DeploymentSecrets {
    /// An empty table with the default cache policy.
    #[must_use]
    pub fn new() -> Self {
        Self {
            table: Arc::new(SecretTable::new()),
        }
    }

    /// The table, for the resolver.
    #[must_use]
    pub fn table(&self) -> Arc<SecretTable> {
        self.table.clone()
    }

    /// Register the stores the host supplies rather than the document.
    pub fn attach_injected(&self, injected: &InjectedSecrets) -> Result<()> {
        for (id, store) in &injected.entries {
            self.table.attach_default(id, store.clone())?;
        }
        Ok(())
    }

    /// The observer that attaches each built store.
    #[must_use]
    pub fn observer(&self) -> BuildObserver {
        let table = self.table.clone();
        Arc::new(move |name: &str, value: &Value| {
            let Ok(handle) = value.clone().downcast::<SecretHandle>() else {
                return;
            };
            let store: Arc<dyn SecretStore> = handle.clone();
            if let Err(error) = table.attach(name, handle.id(), handle.is_default(), store) {
                // The table keeps the first failure and reports it through
                // `check` (and to the next expansion); the build itself cannot
                // fail from inside an observer.
                tracing::warn!(
                    flowcraft.resource = name,
                    %error,
                    "the deployment secret store was not registered"
                );
            }
        })
    }

    /// Fail when a store could not be registered.
    ///
    /// Called after the graph is built: a conflicting default or a duplicate id
    /// must fail the deployment even when nothing resolved a secret.
    pub fn check(&self) -> Result<()> {
        match self.table.failure() {
            Some(error) => Err(error),
            None => Ok(()),
        }
    }
}

/// Stores the process owns, injected before the graph builds.
#[derive(Default)]
pub struct InjectedSecrets {
    entries: Vec<(String, Arc<dyn SecretStore>)>,
}

impl InjectedSecrets {
    /// No injected stores.
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    /// Inject a store as the default (`${secret:NAME}` resolves through it).
    #[must_use]
    pub fn with_default(mut self, id: impl Into<String>, store: Arc<dyn SecretStore>) -> Self {
        self.entries.push((id.into(), store));
        self
    }

    /// Whether anything was injected.
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.entries.is_empty()
    }

    /// The injected entries, in declaration order.
    #[must_use]
    pub fn entries(&self) -> &[(String, Arc<dyn SecretStore>)] {
        &self.entries
    }
}

impl std::fmt::Debug for InjectedSecrets {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("InjectedSecrets")
            .field(
                "ids",
                &self.entries.iter().map(|(id, _)| id).collect::<Vec<_>>(),
            )
            .finish()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use async_trait::async_trait;
    use flowcraft::caps::secret::{MemoryStore, Store as SecretStoreCap};
    use flowcraft::error::Error;
    use flowcraft::resource::Secret;

    struct MapStore(std::collections::BTreeMap<String, String>);

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

    fn handle(store: MemoryStore, id: Option<&str>, default: bool) -> Arc<SecretHandle> {
        let store: Arc<dyn SecretStoreCap> = Arc::new(store);
        Arc::new(SecretHandle::with_settings(
            store,
            id.map(str::to_owned),
            default,
        ))
    }

    #[tokio::test]
    async fn the_observer_attaches_id_and_default() {
        let secrets = DeploymentSecrets::new();
        let observer = secrets.observer();
        observer(
            "primary",
            &(handle(
                MemoryStore::new([("token".to_owned(), "s1".to_owned())]),
                Some("team"),
                true,
            ) as Value),
        );
        let table = secrets.table();
        assert_eq!(table.ids(), vec!["team".to_owned()]);
        assert_eq!(table.default_id().as_deref(), Some("team"));
        let secret = Secret::reference("team", "token");
        assert_eq!(
            secret.resolve(Some(&table)).await.unwrap(),
            "s1",
            "the attached store resolves what the graph references"
        );
        secrets.check().unwrap();
    }

    #[test]
    fn a_second_default_store_fails_the_deployment() {
        let secrets = DeploymentSecrets::new();
        let observer = secrets.observer();
        observer("a", &(handle(MemoryStore::default(), None, true) as Value));
        observer("b", &(handle(MemoryStore::default(), None, true) as Value));
        let error = secrets.check().unwrap_err();
        assert_eq!(error.kind(), flowcraft::error::ErrorKind::Conflict);
        assert!(error.to_string().contains("default"), "{error}");
    }

    #[test]
    fn a_duplicate_id_fails_the_deployment() {
        let secrets = DeploymentSecrets::new();
        let observer = secrets.observer();
        observer(
            "a",
            &(handle(MemoryStore::default(), Some("shared"), false) as Value),
        );
        observer(
            "b",
            &(handle(MemoryStore::default(), Some("shared"), false) as Value),
        );
        let error = secrets.check().unwrap_err();
        assert_eq!(error.kind(), flowcraft::error::ErrorKind::Conflict);
        assert!(error.to_string().contains("shared"), "{error}");
    }

    #[tokio::test]
    async fn a_host_store_can_be_the_default_without_a_resource() {
        let secrets = DeploymentSecrets::new();
        let injected =
            InjectedSecrets::new().with_default("process", MapStore::new([("token", "host")]));
        secrets.attach_injected(&injected).unwrap();
        let secret = Secret::reference("process", "token");
        assert_eq!(
            secret.resolve(Some(&secrets.table())).await.unwrap(),
            "host"
        );
        // It is the default, so a NAME-only reference finds it too.
        let plain = Secret::reference("", "token");
        assert_eq!(plain.resolve(Some(&secrets.table())).await.unwrap(), "host");
    }

    #[tokio::test]
    async fn a_document_default_conflicts_with_an_injected_one() {
        let secrets = DeploymentSecrets::new();
        let injected =
            InjectedSecrets::new().with_default("process", MapStore::new([("token", "host")]));
        secrets.attach_injected(&injected).unwrap();
        secrets.observer()(
            "doc",
            &(handle(MemoryStore::default(), None, true) as Value),
        );
        assert_eq!(
            secrets.check().unwrap_err().kind(),
            flowcraft::error::ErrorKind::Conflict
        );
    }

    #[test]
    fn a_non_store_value_is_ignored() {
        let secrets = DeploymentSecrets::new();
        let observer = secrets.observer();
        observer("other", &(Arc::new(7u8) as Value));
        assert!(secrets.table().ids().is_empty());
        assert!(secrets.check().is_ok());
    }

    #[test]
    fn errors_from_the_observer_are_kept_for_check() {
        let secrets = DeploymentSecrets::new();
        secrets
            .table()
            .record_failure(Error::conflict("a duplicate somewhere"));
        assert!(secrets.check().is_err());
    }
}
