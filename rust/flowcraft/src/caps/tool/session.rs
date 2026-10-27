//! Per-session injection view over a [`Catalog`].
//!
//! Ported from `core/tool/session.go`: the dynamic session applies exposure,
//! the discovery pool, and the per-round budget on the read side; the static
//! session shows every tool and treats stateful operations as no-ops.

use std::collections::{BTreeMap, BTreeSet};
use std::sync::{Arc, Mutex, MutexGuard};

use crate::error::{Error, Result};
use crate::proto::message as msg;
use async_trait::async_trait;

use crate::caps::tool::callable::Tool;
use crate::caps::tool::exposure::{Exposure, Policy};
use crate::caps::tool::registry::Catalog;
use crate::caps::tool::search::{CatalogIndex, DEFAULT_SEARCH_LIMIT, SearchHit};
use crate::caps::tool::state::{DiscoveredEntry, SessionState};
use crate::caps::tool::visibility::{Candidate, fit_discovery_pool, visible_candidates};
use crate::message::definition_bytes;

/// Why a discovered name will not reach the model.
#[derive(Debug, Clone, Copy, PartialEq, Eq, serde::Serialize)]
#[serde(rename_all = "snake_case")]
pub enum DiscoverReason {
    /// No tool with that name.
    Unknown,
    /// The exposure forbids discovery (`always` / `hidden`).
    NotDiscoverable,
    /// Evicted from the pool to fit the budget.
    OverBudget,
    /// In the pool but cut by this round's visible budget.
    VisibleBudget,
}

/// One name's discovery outcome.
#[derive(Debug, Clone, serde::Serialize)]
pub struct DiscoverResult {
    /// Tool name.
    pub name: String,
    /// Whether the next definitions call will include it.
    pub exposed: bool,
    /// Why it will not, when `exposed` is false.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub reason: Option<DiscoverReason>,
}

/// The summary of one [`Session::discover`] call.
#[derive(Debug, Clone, Default, serde::Serialize)]
pub struct DiscoverOutcome {
    /// One entry per requested name, in input order.
    pub results: Vec<DiscoverResult>,
    /// Pool entries dropped by budget pressure.
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub evicted: Vec<String>,
}

/// The per-run injection view.
#[async_trait]
pub trait Session: Catalog {
    /// Add names to the required-by-name set. Idempotent.
    fn require(&self, names: &[&str]);

    /// Add or refresh discovery entries, evicting LRU until the pool fits.
    fn discover(&self, names: &[&str]) -> DiscoverOutcome;

    /// Record a model call, refreshing the tool's discovery recency.
    fn record_call(&self, name: &str);

    /// Advance one round: expire idle entries, drop tools that left the
    /// catalog, and re-enforce the pool cap.
    fn advance_turn(&self);

    /// Rank searchable tools against a query. Honors `policy.search_with_load`
    /// when a policy is configured.
    async fn search(&self, query: &str, limit: usize) -> Result<Vec<SearchHit>>;

    /// Rank with every deferred tool loaded first, regardless of policy.
    async fn search_with_load(&self, query: &str, limit: usize) -> Result<Vec<SearchHit>>;

    /// Eagerly load deferred tools. A no-op without lazy sources.
    async fn load(&self) -> Result<()> {
        Ok(())
    }

    /// Ensure the named tools are loaded. A no-op without lazy sources.
    async fn ensure_loaded(&self, _names: &[&str]) -> Result<()> {
        Ok(())
    }
}

/// The stateful injection view.
pub struct DynamicSession {
    catalog: Arc<dyn Catalog>,
    policy: Policy,
    state: Mutex<SessionState>,
    /// The search index, valid for exactly one catalog snapshot.
    ///
    /// A query arrives per `tool_search` call while the catalog only changes
    /// when the registry does, so indexing per query repeated work on the hot
    /// path: tokenizing, term frequencies and the inverted index for every
    /// searchable tool, on every call. The key is the snapshot itself
    /// (`Arc::ptr_eq`): `Catalog::definitions` is documented to return the same
    /// `Arc` for unchanged contents, which is precisely "the index is still
    /// right".
    rank: Mutex<Option<RankCache>>,
}

/// The index built for one catalog snapshot.
struct RankCache {
    /// The snapshot the index was built from: the cache key.
    snapshot: Arc<[msg::ToolDefinition]>,
    index: Arc<CatalogIndex>,
}

impl DynamicSession {
    /// Build a session. The policy is normalized and validated.
    pub fn new(catalog: Arc<dyn Catalog>, policy: Policy) -> Result<Self> {
        let policy = policy.normalized();
        policy.validate()?;
        Ok(Self {
            catalog,
            policy,
            state: Mutex::new(SessionState::default()),
            rank: Mutex::new(None),
        })
    }

    /// The effective policy.
    #[must_use]
    pub fn policy(&self) -> &Policy {
        &self.policy
    }

    fn state(&self) -> MutexGuard<'_, SessionState> {
        self.state
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// The tools that reach the model this round, borrowed from `snapshot`.
    ///
    /// Borrowed rather than owned: the caller decides whether it needs owned
    /// definitions (a wire list does, a round's visibility check does not), and
    /// only the ones that survive the budget are worth copying.
    fn visible_set<'a>(
        &self,
        snapshot: &'a [msg::ToolDefinition],
        policy: &Policy,
        state: &crate::caps::tool::state::Snapshot,
    ) -> Vec<Candidate<'a>> {
        let sizes = pool_sizes(snapshot, state);
        let candidates: Vec<Candidate<'a>> = snapshot
            .iter()
            .map(|definition| Candidate {
                name: definition.name.as_str(),
                exposure: policy.exposure_of(&definition.name),
                definition,
            })
            .collect();
        let fitted = fit_discovery_pool(state, &sizes, policy.discovery);
        visible_candidates(candidates, &fitted, policy)
    }

    fn evict_for_fit(&self, state: &mut SessionState) -> Vec<String> {
        let pool = self.policy.discovery;
        let mut evicted = Vec::new();
        loop {
            let (tools, total) = state.discovered_totals();
            if tools <= pool.max_tools && (total <= pool.max_bytes || tools <= 1) {
                return evicted;
            }
            if tools == 0 {
                return evicted;
            }
            let snapshot = state.snapshot();
            let mut victim: Option<(String, DiscoveredEntry, bool)> = None;
            for (name, entry) in &snapshot.discovered {
                let deferred = self.policy.exposure_of(name) == Exposure::Deferred;
                victim = match victim {
                    None => Some((name.clone(), *entry, deferred)),
                    Some((_, previous, previous_deferred))
                        if evicts_before(*entry, deferred, previous, previous_deferred) =>
                    {
                        Some((name.clone(), *entry, deferred))
                    }
                    other => other,
                };
            }
            let Some((name, _, _)) = victim else {
                return evicted;
            };
            state.remove_discovered(&name);
            evicted.push(name);
        }
    }
}

fn evicts_before(
    candidate: DiscoveredEntry,
    candidate_deferred: bool,
    other: DiscoveredEntry,
    other_deferred: bool,
) -> bool {
    if candidate.last_use != other.last_use {
        return candidate.last_use < other.last_use;
    }
    if candidate_deferred != other_deferred {
        return candidate_deferred;
    }
    candidate.seq < other.seq
}

impl Catalog for DynamicSession {
    fn get(&self, name: &str) -> Option<Arc<dyn Tool>> {
        self.catalog.get(name)
    }

    fn definitions(&self) -> Arc<[msg::ToolDefinition]> {
        let state = self.state().snapshot();
        let definitions = self.catalog.definitions();
        // Only the survivors are copied out of the shared snapshot: a catalog of
        // 500 tools narrowed to a 30-tool budget clones 30 definitions, not 500.
        self.visible_set(&definitions, &self.policy, &state)
            .into_iter()
            .map(|candidate| candidate.definition.clone())
            .collect()
    }
}

/// Definition sizes for the names the pool fit can consult.
///
/// `fit_discovery_pool` only ever looks up `state.discovered`, so building the
/// map for the whole catalog was pure overhead on every round. The snapshot is
/// sorted by name, so each lookup is a binary search.
fn pool_sizes(
    snapshot: &[msg::ToolDefinition],
    state: &crate::caps::tool::state::Snapshot,
) -> BTreeMap<String, i64> {
    state
        .discovered
        .keys()
        .filter_map(|name| {
            snapshot
                .binary_search_by(|definition| definition.name.as_str().cmp(name))
                .ok()
                .map(|at| (name.clone(), definition_bytes(&snapshot[at])))
        })
        .collect()
}

#[async_trait]
impl Session for DynamicSession {
    fn require(&self, names: &[&str]) {
        self.state().require(names);
    }

    fn discover(&self, names: &[&str]) -> DiscoverOutcome {
        let mut state = self.state();
        let mut reasons: BTreeMap<String, DiscoverReason> = BTreeMap::new();
        let mut changed = false;
        let mut rank = 0;
        for name in names {
            let Some(tool) = self.catalog.get(name) else {
                reasons.insert((*name).to_owned(), DiscoverReason::Unknown);
                continue;
            };
            if !self.policy.exposure_of(name).searchable() {
                reasons.insert((*name).to_owned(), DiscoverReason::NotDiscoverable);
                continue;
            }
            state.touch(name, definition_bytes(&tool.definition()), rank);
            rank += 1;
            changed = true;
        }

        let evicted = if changed {
            self.evict_for_fit(&mut state)
        } else {
            Vec::new()
        };
        let visible: BTreeSet<String> = if changed {
            let snapshot = state.snapshot();
            let definitions = self.catalog.definitions();
            self.visible_set(&definitions, &self.policy, &snapshot)
                .into_iter()
                .map(|candidate| candidate.name.to_owned())
                .collect()
        } else {
            BTreeSet::new()
        };

        let results = names
            .iter()
            .map(|name| {
                if let Some(reason) = reasons.get(*name) {
                    return DiscoverResult {
                        name: (*name).to_owned(),
                        exposed: false,
                        reason: Some(*reason),
                    };
                }
                if !state.is_discovered(name) {
                    return DiscoverResult {
                        name: (*name).to_owned(),
                        exposed: false,
                        reason: Some(DiscoverReason::OverBudget),
                    };
                }
                if !visible.contains(*name) {
                    return DiscoverResult {
                        name: (*name).to_owned(),
                        exposed: false,
                        reason: Some(DiscoverReason::VisibleBudget),
                    };
                }
                DiscoverResult {
                    name: (*name).to_owned(),
                    exposed: true,
                    reason: None,
                }
            })
            .collect();
        DiscoverOutcome { results, evicted }
    }

    fn record_call(&self, name: &str) {
        if !self.policy.exposure_of(name).searchable() {
            return;
        }
        let Some(tool) = self.catalog.get(name) else {
            return;
        };
        let mut state = self.state();
        state.touch(name, definition_bytes(&tool.definition()), 0);
        self.evict_for_fit(&mut state);
    }

    fn advance_turn(&self) {
        let mut state = self.state();
        state.advance_turn(self.policy.discovery.idle_rounds, |name| {
            self.catalog.get(name).is_some()
        });
        self.evict_for_fit(&mut state);
    }

    async fn search(&self, query: &str, limit: usize) -> Result<Vec<SearchHit>> {
        if self.policy.search_with_load {
            // A failed preload degrades ranking quality, not correctness.
            let _ = self.load().await;
        }
        self.rank(query, limit)
    }

    async fn search_with_load(&self, query: &str, limit: usize) -> Result<Vec<SearchHit>> {
        let _ = self.load().await;
        self.rank(query, limit)
    }

    async fn load(&self) -> Result<()> {
        let mut first: Option<Error> = None;
        // The snapshot is shared: only the names are read here, nothing is copied
        // out of it.
        let definitions = self.catalog.definitions();
        for definition in definitions.iter() {
            if let Some(tool) = self.catalog.get(&definition.name)
                && let Err(error) = tool.ensure_loaded().await
                && first.is_none()
            {
                first = Some(error);
            }
        }
        match first {
            Some(error) => Err(error),
            None => Ok(()),
        }
    }

    async fn ensure_loaded(&self, names: &[&str]) -> Result<()> {
        let mut first: Option<Error> = None;
        for name in names {
            if let Some(tool) = self.catalog.get(name)
                && let Err(error) = tool.ensure_loaded().await
                && first.is_none()
            {
                first = Some(error);
            }
        }
        match first {
            Some(error) => Err(error),
            None => Ok(()),
        }
    }
}

impl DynamicSession {
    fn rank(&self, query: &str, limit: usize) -> Result<Vec<SearchHit>> {
        let definitions = self.catalog.definitions();
        let mut limit = if limit == 0 {
            DEFAULT_SEARCH_LIMIT
        } else {
            limit
        };
        if self.policy.discovery.max_tools > 0 {
            limit = limit.min(self.policy.discovery.max_tools);
        }
        // A hit is the common case: the catalog does not change between rounds.
        if let Ok(cache) = self.rank.lock()
            && let Some(cache) = cache.as_ref()
            && Arc::ptr_eq(&cache.snapshot, &definitions)
        {
            return cache.index.search(query, limit);
        }
        // Only the searchable tools are copied out: the filter used to run after
        // the whole catalog had already been cloned.
        let docs: Vec<(String, String)> = definitions
            .iter()
            .filter(|definition| self.policy.exposure_of(&definition.name).searchable())
            .map(|definition| (definition.name.clone(), definition.description.clone()))
            .collect();
        let index = Arc::new(CatalogIndex::new(docs)?);
        let hits = index.search(query, limit);
        if let Ok(mut cache) = self.rank.lock() {
            *cache = Some(RankCache {
                snapshot: definitions,
                index,
            });
        }
        hits
    }
}

/// The non-dynamic view: every tool visible, no state.
pub struct StaticSession {
    catalog: Arc<dyn Catalog>,
}

impl StaticSession {
    /// Wrap a catalog.
    #[must_use]
    pub fn new(catalog: Arc<dyn Catalog>) -> Self {
        Self { catalog }
    }
}

impl Catalog for StaticSession {
    fn get(&self, name: &str) -> Option<Arc<dyn Tool>> {
        self.catalog.get(name)
    }

    fn definitions(&self) -> Arc<[msg::ToolDefinition]> {
        self.catalog.definitions()
    }
}

#[async_trait]
impl Session for StaticSession {
    fn require(&self, _names: &[&str]) {}

    fn discover(&self, _names: &[&str]) -> DiscoverOutcome {
        DiscoverOutcome::default()
    }

    fn record_call(&self, _name: &str) {}

    fn advance_turn(&self) {}

    async fn search(&self, _query: &str, _limit: usize) -> Result<Vec<SearchHit>> {
        Err(Error::not_available(
            "tool: dynamic injection is not enabled on this assembly",
        ))
    }

    async fn search_with_load(&self, _query: &str, _limit: usize) -> Result<Vec<SearchHit>> {
        Err(Error::not_available(
            "tool: dynamic injection is not enabled on this assembly",
        ))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::caps::tool::callable::FuncTool;
    use crate::caps::tool::registry::{ConflictPolicy, Registry};
    use crate::message::text_content;

    fn definition(name: &str, description: &str) -> msg::ToolDefinition {
        msg::ToolDefinition {
            name: name.to_owned(),
            description: description.to_owned(),
            input_schema: Vec::new(),
        }
    }

    async fn registry(names: &[(&str, &str)]) -> Arc<Registry> {
        let registry = Registry::from_sources(&[], ConflictPolicy::Error)
            .await
            .expect("registry");
        for (name, description) in names {
            registry
                .add(Arc::new(FuncTool::new(
                    definition(name, description),
                    Arc::new(|_context, _| Ok(text_content("ok"))),
                )))
                .expect("add");
        }
        registry
    }

    fn policy(default: Exposure) -> Policy {
        Policy {
            default,
            ..Policy::default()
        }
    }

    #[tokio::test]
    async fn definitions_show_always_and_hide_deferred() {
        let registry = registry(&[("always", ""), ("deferred", "")]).await;
        let mut policy = policy(Exposure::Deferred);
        policy
            .exposures
            .insert("always".to_owned(), Exposure::Always);
        let session = DynamicSession::new(registry, policy).expect("session");
        assert_eq!(
            session
                .definitions()
                .iter()
                .map(|d| d.name.as_str())
                .collect::<Vec<_>>(),
            vec!["always"]
        );
    }

    #[tokio::test]
    async fn discover_makes_a_tool_visible_and_reports_unknowns() {
        let registry = registry(&[("grep", "search files")]).await;
        let session = DynamicSession::new(registry, policy(Exposure::Deferred)).expect("session");
        let outcome = session.discover(&["grep", "nope"]);
        assert!(outcome.results[0].exposed);
        assert_eq!(outcome.results[1].reason, Some(DiscoverReason::Unknown));
        assert_eq!(session.definitions()[0].name, "grep");
        assert_eq!(session.search("search", 8).await.unwrap()[0].name, "grep");
    }

    #[tokio::test]
    async fn advancing_a_turn_expires_idle_entries() {
        let registry = registry(&[("grep", "")]).await;
        let mut policy = policy(Exposure::Deferred);
        policy.discovery.idle_rounds = 1;
        let session = DynamicSession::new(registry, policy).expect("session");
        session.discover(&["grep"]);
        session.advance_turn();
        session.advance_turn();
        assert!(session.definitions().is_empty());
    }

    #[tokio::test]
    async fn the_static_session_hides_search() {
        let registry = registry(&[("grep", "")]).await;
        let session = StaticSession::new(registry);
        assert_eq!(session.definitions().len(), 1);
        assert!(session.search("grep", 8).await.is_err());
    }
}
