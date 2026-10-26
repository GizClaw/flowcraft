//! Deployment secrets: lazy references and the store table behind them.
//!
//! `${secret:NAME}` and `${secret:ID.NAME}` do not expand to a value while the
//! graph is built. They expand to a [`SecretRef`] marker, which a settings field
//! typed as [`Secret`] decodes; the value is looked up when the consumer
//! actually needs it, through the [`SecretTable`] the deployment assembled.
//!
//! Two properties follow from that, and both are the point:
//!
//! * A secret never appears in a settings tree, a log line, or an error message
//!   on its own: [`Secret`] renders as `<secret>` in `Debug`, `Display`, and
//!   `Serialize`.
//! * A missing secret fails the request that needs it, not the whole build.
//!
//! Stores are `Pre`-phase resources. The host attaches each one as soon as it
//! is built (see [`crate::resource::BuildObserver`]), so every `Main` resource expands its
//! references against a populated table.

use std::collections::BTreeMap;
use std::sync::{Arc, RwLock};
use std::time::{Duration, Instant};

use crate::error::{Error, Result};
use async_trait::async_trait;
use serde::{Deserialize, Deserializer, Serialize, Serializer};
use serde_json::{Map, Value};

use crate::resource::resolver::SecretStore;

/// The key a [`Secret`] marker carries, so a settings object written by hand
/// (`{"store": ..., "name": ...}`, the Go wire shape) is never mistaken for one.
pub const MARKER: &str = "$secret";

/// How long a resolved value is reused by default.
pub const DEFAULT_TTL: Duration = Duration::from_secs(60);

/// How many names one store's cache holds by default.
pub const DEFAULT_CAPACITY: usize = 256;

/// The store id the resolver falls back to when a [`SecretTable`] is built from
/// one unnamed store ([`SecretTable::single`]).
const UNNAMED: &str = "";

/// A resolved-by-proxy secret: which store, which name.
#[derive(Debug, Clone, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SecretRef {
    /// The store id (its settings `id`, or its resource name).
    pub store: String,
    /// The secret name inside that store.
    pub name: String,
}

impl SecretRef {
    /// Build one.
    #[must_use]
    pub fn new(store: impl Into<String>, name: impl Into<String>) -> Self {
        Self {
            store: store.into(),
            name: name.into(),
        }
    }
}

/// A settings value that is either a literal string or a lazy secret reference.
///
/// String forms:
///
/// * `"plain"` — a literal.
/// * `{"$secret": {"store": "...", "name": "..."}}` — what the resolver emits.
/// * `{"store": "...", "name": "..."}` — the Go-side shape, accepted so a
///   document written against the Go API keeps working.
///
/// Everything that prints a `Secret` prints `<secret>`, including `Serialize`,
/// so a value cannot leak through a log line or a snapshot.
#[derive(Clone, Default, PartialEq, Eq)]
pub struct Secret {
    literal: Option<String>,
    reference: Option<SecretRef>,
}

impl Secret {
    /// Wrap a plain value.
    #[must_use]
    pub fn literal(value: impl Into<String>) -> Self {
        Self {
            literal: Some(value.into()),
            reference: None,
        }
    }

    /// Wrap a reference that resolves through `table`.
    #[must_use]
    pub fn reference(store: impl Into<String>, name: impl Into<String>) -> Self {
        Self {
            literal: None,
            reference: Some(SecretRef::new(store, name)),
        }
    }

    /// Whether this is a lazy reference.
    #[must_use]
    pub fn is_ref(&self) -> bool {
        self.reference.is_some()
    }

    /// Whether the value carries nothing: no literal and no reference.
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.literal.is_none() && self.reference.is_none()
    }

    /// The literal text, when this is not a reference.
    #[must_use]
    pub fn as_literal(&self) -> Option<&str> {
        self.literal.as_deref()
    }

    /// The reference, when this is one.
    #[must_use]
    pub fn as_ref(&self) -> Option<&SecretRef> {
        self.reference.as_ref()
    }

    /// Resolve to the value.
    ///
    /// A literal returns itself. A reference needs the table the deployment
    /// installed; without one there is nothing to resolve it against.
    pub async fn resolve(&self, table: Option<&SecretTable>) -> Result<String> {
        match (&self.literal, &self.reference) {
            (Some(value), _) => Ok(value.clone()),
            (None, Some(reference)) => {
                let table = table.ok_or_else(|| {
                    Error::not_available(format!(
                        "secret {:?} in store {:?} needs a secret store table",
                        reference.name, reference.store
                    ))
                })?;
                table.resolve(&reference.store, &reference.name).await
            }
            (None, None) => Err(Error::validation(
                "secret value is neither literal nor reference",
            )),
        }
    }
}

impl std::fmt::Debug for Secret {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("<secret>")
    }
}

impl std::fmt::Display for Secret {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("<secret>")
    }
}

impl Serialize for Secret {
    fn serialize<S: Serializer>(&self, serializer: S) -> std::result::Result<S::Ok, S::Error> {
        serializer.serialize_str("<secret>")
    }
}

impl<'de> Deserialize<'de> for Secret {
    fn deserialize<D: Deserializer<'de>>(deserializer: D) -> std::result::Result<Self, D::Error> {
        let value = Value::deserialize(deserializer)?;
        Self::from_value(&value).map_err(serde::de::Error::custom)
    }
}

impl Secret {
    /// Decode one settings value.
    fn from_value(value: &Value) -> Result<Self> {
        match value {
            Value::String(text) => Ok(Self::literal(text.clone())),
            Value::Object(map) => {
                let body = match map.get(MARKER) {
                    Some(inner) => inner.as_object().ok_or_else(|| {
                        Error::validation(format!("secret {MARKER} marker must be an object"))
                    })?,
                    None => map,
                };
                let store = string_field(body, "store")?;
                let name = string_field(body, "name")?.unwrap_or_default();
                if name.trim().is_empty() {
                    return Err(Error::validation("secret reference: name is empty"));
                }
                Ok(Self::reference(
                    store.unwrap_or_else(|| UNNAMED.to_owned()),
                    name,
                ))
            }
            other => Err(Error::validation(format!(
                "secret: want a string or {{\"$secret\":{{\"store\":..,\"name\":..}}}}, got {other}"
            ))),
        }
    }
}

fn string_field(map: &Map<String, Value>, key: &str) -> Result<Option<String>> {
    match map.get(key) {
        None => Ok(None),
        Some(Value::String(text)) => Ok(Some(text.clone())),
        Some(other) => Err(Error::validation(format!(
            "secret reference: {key} must be a string, got {other}"
        ))),
    }
}

/// A recorded failure. [`Error`] is not `Clone`, and the table has to hand the
/// same failure to every later caller, so it keeps the classification and the
/// message and rebuilds one on demand.
#[derive(Debug)]
struct Failure {
    kind: crate::error::ErrorKind,
    message: String,
}

impl Failure {
    fn of(error: &Error) -> Self {
        Self {
            kind: error.kind(),
            message: error.to_string(),
        }
    }

    fn error(&self) -> Error {
        Error::new(self.kind, self.message.clone())
    }
}

#[derive(Debug)]
struct CacheEntry {
    value: String,
    found: bool,
    expires: Instant,
}

impl CacheEntry {
    fn live(&self, now: Instant) -> bool {
        self.expires > now
    }
}

impl std::fmt::Display for CacheEntry {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("<secret cache entry>")
    }
}

#[derive(Default)]
struct TableState {
    stores: BTreeMap<String, Arc<dyn SecretStore>>,
    default: Option<String>,
    /// The first attach failure, kept so the next expansion reports it instead of
    /// silently resolving against a half-assembled table.
    failure: Option<Failure>,
    cache: BTreeMap<String, BTreeMap<String, CacheEntry>>,
}

impl std::fmt::Debug for TableState {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("TableState")
            .field("stores", &self.stores.keys().collect::<Vec<_>>())
            .field("default", &self.default)
            .field("failure", &self.failure)
            .finish_non_exhaustive()
    }
}

/// The deployment's secret stores, keyed by id.
///
/// Shared between the resolver (which needs the ids at expansion time) and the
/// build observer (which attaches each `secret.Store` as it is built). Every
/// handle is the same table, so attaching and resolving see one state.
pub struct SecretTable {
    state: RwLock<TableState>,
    ttl: Duration,
    capacity: usize,
}

impl std::fmt::Debug for SecretTable {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self.state.read() {
            Ok(state) => f
                .debug_struct("SecretTable")
                .field("state", &*state)
                .finish(),
            Err(_) => f.write_str("SecretTable(<poisoned>)"),
        }
    }
}

impl Default for SecretTable {
    fn default() -> Self {
        Self::new()
    }
}

impl SecretTable {
    /// An empty table with the default cache policy.
    #[must_use]
    pub fn new() -> Self {
        Self {
            state: RwLock::new(TableState::default()),
            ttl: DEFAULT_TTL,
            capacity: DEFAULT_CAPACITY,
        }
    }

    /// A table holding one unnamed store, which every reference resolves
    /// through. This is the single-store shortcut behind
    /// [`crate::resource::Resolver::with_secrets`].
    #[must_use]
    pub fn single(store: Arc<dyn SecretStore>) -> Self {
        let table = Self::new();
        if let Ok(mut state) = table.state.write() {
            state.stores.insert(UNNAMED.to_owned(), store);
            state.default = Some(UNNAMED.to_owned());
        }
        table
    }

    /// Change how long resolved values are reused. Zero disables caching.
    #[must_use]
    pub fn with_ttl(mut self, ttl: Duration) -> Self {
        self.ttl = ttl;
        self
    }

    /// Change how many names one store's cache holds.
    #[must_use]
    pub fn with_capacity(mut self, capacity: usize) -> Self {
        if capacity > 0 {
            self.capacity = capacity;
        }
        self
    }

    /// Register a built store.
    ///
    /// `resource` is the document name; `id` is the settings `id`, which falls
    /// back to the resource name. At most one store may be the default.
    pub fn attach(
        &self,
        resource: &str,
        id: Option<&str>,
        default: bool,
        store: Arc<dyn SecretStore>,
    ) -> Result<()> {
        // An empty `id` falls back to the resource name; a padded one is a
        // mistake worth reporting rather than silently normalizing away.
        let id = match id {
            Some("") => resource.to_owned(),
            Some(id) => id.to_owned(),
            None => resource.to_owned(),
        };
        validate_id(&id)?;
        let mut state = self
            .state
            .write()
            .map_err(|_| Error::internal("secret table lock is poisoned"))?;
        if state.stores.contains_key(&id) {
            return Err(self.record(
                &mut state,
                Error::conflict(format!(
                    "secret store id {id:?} is already registered (resource {resource:?})"
                )),
            ));
        }
        if default {
            if let Some(existing) = state.default.clone() {
                return Err(self.record(
                    &mut state,
                    Error::conflict(format!(
                        "secret store {id:?} declares default, but {existing:?} already is; at most one store may be the default"
                    )),
                ));
            }
            state.default = Some(id.clone());
        }
        state.stores.insert(id, store);
        Ok(())
    }

    /// Register a store as the default, failing when one already is.
    ///
    /// This is the host-injection entry point (a store the process owns rather
    /// than one the document builds).
    pub fn attach_default(&self, id: &str, store: Arc<dyn SecretStore>) -> Result<()> {
        self.attach(id, Some(id), true, store)
    }

    /// The first failure recorded while attaching, if any.
    pub fn failure(&self) -> Option<Error> {
        self.state
            .read()
            .ok()
            .and_then(|state| state.failure.as_ref().map(Failure::error))
    }

    /// Record a failure that happened outside [`Self::attach`] (the observer
    /// cannot return an error).
    pub fn record_failure(&self, error: Error) {
        if let Ok(mut state) = self.state.write() {
            Self::record(self, &mut state, error);
        }
    }

    fn record(&self, state: &mut TableState, error: Error) -> Error {
        if state.failure.is_none() {
            state.failure = Some(Failure::of(&error));
        }
        error
    }

    /// Whether an id names a store.
    #[must_use]
    pub fn knows(&self, id: &str) -> bool {
        self.state
            .read()
            .map(|state| state.stores.contains_key(id))
            .unwrap_or(false)
    }

    /// Every registered store id, sorted.
    #[must_use]
    pub fn ids(&self) -> Vec<String> {
        self.state
            .read()
            .map(|state| state.stores.keys().cloned().collect())
            .unwrap_or_default()
    }

    /// The default store id, when one is registered.
    #[must_use]
    pub fn default_id(&self) -> Option<String> {
        self.state
            .read()
            .ok()
            .and_then(|state| state.default.clone())
    }

    /// Resolve one name through one store, caching the answer.
    ///
    /// An empty `store` means the default store, which is what a marker without
    /// an explicit id carries.
    pub async fn resolve(&self, store: &str, name: &str) -> Result<String> {
        if let Some(error) = self.failure() {
            return Err(error);
        }
        if name.trim().is_empty() {
            return Err(Error::validation("secret name is empty"));
        }
        let store = if store.is_empty() {
            self.default_id()
                .ok_or_else(|| Error::validation(no_default_message(name)))?
        } else {
            store.to_owned()
        };
        let store = store.as_str();
        let handle = {
            let state = self
                .state
                .read()
                .map_err(|_| Error::internal("secret table lock is poisoned"))?;
            state.stores.get(store).cloned().ok_or_else(|| {
                Error::not_found(format!("secret store {store:?} is not configured"))
            })?
        };
        if self.ttl.is_zero() {
            return found(handle.lookup(name).await?, store, name);
        }
        let now = Instant::now();
        if let Some(hit) = self.cached(store, name, now) {
            return hit;
        }
        let (value, found) = match handle.lookup(name).await? {
            Some(value) => (value, true),
            None => (String::new(), false),
        };
        self.store(store, name, &value, found, now);
        if found {
            Ok(value)
        } else {
            Err(not_found(store, name))
        }
    }

    /// Resolve through the default store (the `${secret:NAME}` form).
    pub async fn resolve_default(&self, name: &str) -> Result<String> {
        let store = self
            .default_id()
            .ok_or_else(|| Error::validation(no_default_message(name)))?;
        self.resolve(&store, name).await
    }

    /// Drop every cached value for one store, or all stores when `store` is
    /// `None`. A management API that writes a secret calls this so the next
    /// request sees the new value instead of waiting out the TTL.
    pub fn invalidate(&self, store: Option<&str>) {
        let Ok(mut state) = self.state.write() else {
            return;
        };
        match store {
            Some(id) => {
                state.cache.remove(id);
            }
            None => state.cache.clear(),
        }
    }

    fn cached(&self, store: &str, name: &str, now: Instant) -> Option<Result<String>> {
        let state = self.state.read().ok()?;
        let entry = state.cache.get(store)?.get(name)?;
        if !entry.live(now) {
            return None;
        }
        Some(if entry.found {
            Ok(entry.value.clone())
        } else {
            Err(not_found(store, name))
        })
    }

    fn store(&self, store: &str, name: &str, value: &str, found: bool, now: Instant) {
        let Ok(mut state) = self.state.write() else {
            return;
        };
        let entries = state.cache.entry(store.to_owned()).or_default();
        if !entries.contains_key(name) && entries.len() >= self.capacity {
            // Evict the entry closest to expiry: a live one is the cheapest to
            // lose, an expired one is free.
            if let Some(victim) = entries
                .iter()
                .min_by_key(|(_, entry)| entry.expires)
                .map(|(key, _)| key.clone())
            {
                entries.remove(&victim);
            }
        }
        entries.insert(
            name.to_owned(),
            CacheEntry {
                value: value.to_owned(),
                found,
                expires: now + self.ttl,
            },
        );
    }
}

fn found(value: Option<String>, store: &str, name: &str) -> Result<String> {
    match value {
        Some(value) => Ok(value),
        None => Err(not_found(store, name)),
    }
}

fn not_found(store: &str, name: &str) -> Error {
    Error::not_found(format!("secret {name:?} is not set in store {store:?}"))
}

fn no_default_message(name: &str) -> String {
    format!(
        "secret reference for {name:?} needs an explicit store or a default secret.Store resource"
    )
}

fn validate_id(id: &str) -> Result<()> {
    if id.is_empty() {
        return Err(Error::validation("secret store id is empty"));
    }
    if id != id.trim() {
        return Err(Error::validation(format!(
            "secret store id {id:?} has surrounding whitespace"
        )));
    }
    if id.contains(['.', '/', '\\', ':', ' ', '\t', '\r', '\n']) {
        return Err(Error::validation(format!(
            "secret store id {id:?} must not contain '.', '/', ':' or whitespace"
        )));
    }
    Ok(())
}

#[async_trait]
impl SecretStore for SecretTable {
    async fn lookup(&self, name: &str) -> Result<Option<String>> {
        match self.resolve_default(name).await {
            Ok(value) => Ok(Some(value)),
            Err(error) if error.kind() == crate::error::ErrorKind::NotFound => Ok(None),
            Err(error) => Err(error),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[derive(Debug)]
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

    fn table() -> SecretTable {
        let table = SecretTable::new();
        table
            .attach(
                "primary",
                Some("team"),
                true,
                MapStore::new([("token", "s1")]),
            )
            .unwrap();
        table
            .attach("other", None, false, MapStore::new([("token", "s2")]))
            .unwrap();
        table
    }

    #[tokio::test]
    async fn resolves_by_id_and_by_default() {
        let table = table();
        assert_eq!(table.resolve("team", "token").await.unwrap(), "s1");
        assert_eq!(table.resolve("other", "token").await.unwrap(), "s2");
        assert_eq!(table.resolve_default("token").await.unwrap(), "s1");
        assert_eq!(table.default_id().as_deref(), Some("team"));
        assert_eq!(table.ids(), vec!["other".to_owned(), "team".to_owned()]);
    }

    #[tokio::test]
    async fn a_missing_store_and_a_missing_name_are_distinct() {
        let table = table();
        let missing = table.resolve("nope", "token").await.unwrap_err();
        assert_eq!(missing.kind(), crate::error::ErrorKind::NotFound);
        assert!(missing.to_string().contains("not configured"), "{missing}");

        let absent = table.resolve("team", "absent").await.unwrap_err();
        assert_eq!(absent.kind(), crate::error::ErrorKind::NotFound);
    }

    #[tokio::test]
    async fn two_defaults_are_a_conflict_and_stick() {
        let table = SecretTable::new();
        table
            .attach("a", None, true, MapStore::new([("x", "1")]))
            .unwrap();
        let error = table
            .attach("b", None, true, MapStore::new([("x", "2")]))
            .unwrap_err();
        assert_eq!(error.kind(), crate::error::ErrorKind::Conflict);
        // The recorded failure surfaces on the next resolution rather than being
        // lost: a half-assembled table must not serve reads.
        let error = table.resolve("a", "x").await.unwrap_err();
        assert_eq!(error.kind(), crate::error::ErrorKind::Conflict);
    }

    #[tokio::test]
    async fn duplicate_ids_are_a_conflict() {
        let table = SecretTable::new();
        table
            .attach("a", Some("shared"), false, MapStore::new([]))
            .unwrap();
        let error = table
            .attach("b", Some("shared"), false, MapStore::new([]))
            .unwrap_err();
        assert_eq!(error.kind(), crate::error::ErrorKind::Conflict);
    }

    #[test]
    fn ids_that_cannot_be_addressed_are_rejected() {
        let table = SecretTable::new();
        for id in ["has.dot", "has/slash", " spaced"] {
            let error = table
                .attach("r", Some(id), false, MapStore::new([]))
                .unwrap_err();
            assert_eq!(
                error.kind(),
                crate::error::ErrorKind::Validation,
                "id {id:?}"
            );
        }
        // An empty `id` setting falls back to the resource name, like Go.
        table
            .attach("fallback", Some(""), false, MapStore::new([]))
            .unwrap();
        assert!(table.knows("fallback"));
    }

    #[tokio::test]
    async fn the_cache_serves_repeats_and_expires() {
        #[derive(Debug)]
        struct Counting(std::sync::atomic::AtomicUsize, Arc<MapStore>);

        #[async_trait]
        impl SecretStore for Counting {
            async fn lookup(&self, name: &str) -> Result<Option<String>> {
                self.0.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
                self.1.lookup(name).await
            }
        }

        let inner = MapStore::new([("token", "s1")]);
        let counter = Arc::new(Counting(
            std::sync::atomic::AtomicUsize::new(0),
            inner.clone(),
        ));
        let table = SecretTable::new().with_ttl(Duration::from_millis(20));
        table.attach("s", None, true, counter.clone()).unwrap();

        assert_eq!(table.resolve("s", "token").await.unwrap(), "s1");
        assert_eq!(table.resolve("s", "token").await.unwrap(), "s1");
        assert_eq!(counter.0.load(std::sync::atomic::Ordering::SeqCst), 1);

        // Negative results are cached too, so a missing name does not hammer a
        // backend on every request.
        assert!(table.resolve("s", "absent").await.is_err());
        assert!(table.resolve("s", "absent").await.is_err());
        assert_eq!(counter.0.load(std::sync::atomic::Ordering::SeqCst), 2);

        tokio::time::sleep(Duration::from_millis(30)).await;
        assert_eq!(table.resolve("s", "token").await.unwrap(), "s1");
        assert_eq!(counter.0.load(std::sync::atomic::Ordering::SeqCst), 3);
    }

    #[tokio::test]
    async fn invalidate_drops_cached_values() {
        /// A store whose value can be swapped behind the cache.
        #[derive(Debug)]
        struct Swappable(std::sync::RwLock<String>);

        #[async_trait]
        impl SecretStore for Swappable {
            async fn lookup(&self, name: &str) -> Result<Option<String>> {
                if name != "token" {
                    return Ok(None);
                }
                Ok(Some(self.0.read().unwrap().clone()))
            }
        }

        let store = Arc::new(Swappable(std::sync::RwLock::new("old".to_owned())));
        let table = SecretTable::new();
        table.attach("s", None, true, store.clone()).unwrap();
        assert_eq!(table.resolve("s", "token").await.unwrap(), "old");

        *store.0.write().unwrap() = "new".to_owned();
        // Cached: the write is not visible yet.
        assert_eq!(table.resolve("s", "token").await.unwrap(), "old");
        table.invalidate(Some("s"));
        assert_eq!(table.resolve("s", "token").await.unwrap(), "new");
    }

    #[test]
    fn a_secret_never_prints_its_value() {
        let secret = Secret::literal("hunter2");
        assert_eq!(format!("{secret}"), "<secret>");
        assert_eq!(format!("{secret:?}"), "<secret>");
        assert_eq!(serde_json::to_string(&secret).unwrap(), "\"<secret>\"");
        assert_eq!(secret.as_literal(), Some("hunter2"));
        assert!(!secret.is_ref());
    }

    #[test]
    fn references_decode_from_every_accepted_shape() {
        let marker = serde_json::json!({"$secret": {"store": "team", "name": "token"}});
        let secret: Secret = serde_json::from_value(marker).unwrap();
        assert_eq!(secret.as_ref(), Some(&SecretRef::new("team", "token")));

        let go_shape = serde_json::json!({"store": "team", "name": "token"});
        let secret: Secret = serde_json::from_value(go_shape).unwrap();
        assert!(secret.is_ref());

        let literal: Secret = serde_json::from_value(serde_json::json!("plain")).unwrap();
        assert_eq!(literal.as_literal(), Some("plain"));

        // An empty name is refused rather than resolved later.
        let empty = serde_json::json!({"$secret": {"store": "team", "name": ""}});
        assert!(serde_json::from_value::<Secret>(empty).is_err());
        assert!(serde_json::from_value::<Secret>(serde_json::json!(3)).is_err());
    }

    #[tokio::test]
    async fn resolving_a_reference_needs_a_table() {
        let secret = Secret::reference("team", "token");
        let error = secret.resolve(None).await.unwrap_err();
        assert_eq!(error.kind(), crate::error::ErrorKind::NotAvailable);
        let table = table();
        assert_eq!(secret.resolve(Some(&table)).await.unwrap(), "s1");
    }

    #[tokio::test]
    async fn a_table_alone_resolves_the_default_store() {
        let table = table();
        let store: Arc<dyn SecretStore> = Arc::new(table);
        assert_eq!(store.lookup("token").await.unwrap(), Some("s1".to_owned()));
        assert_eq!(store.lookup("absent").await.unwrap(), None);
    }
}
