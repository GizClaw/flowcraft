//! The registry: many sources merged into one catalog, with a runtime
//! registration surface for sources that discover tools late.
//!
//! Ported from `core/tool/registry.go`. Two Go mechanisms are adapted rather
//! than copied:
//!
//! * Go's `io.Closer` becomes [`Tool::close`], called by [`Registry::close`] in
//!   reverse registration order.
//! * Go's `RegistryAttacher`/`RegistryDetacher` become [`Source::attach`] and
//!   [`Source::detach`]. The registrar holds a [`Weak`] reference to the
//!   registry's inner state, so a source outliving its registry cannot keep it
//!   (and its tools) alive; `detach` remains for sources that need to stop
//!   background work.

use std::collections::BTreeMap;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, OnceLock, RwLock, Weak};

use crate::error::{Error, Result};
use crate::proto::message as msg;
use crate::resource::BoxFuture;
use async_trait::async_trait;

use crate::caps::tool::callable::{Tool, ToolMeta};

/// What happens when two sources contribute the same tool name.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub enum ConflictPolicy {
    /// Fail construction. The default: two sources disagreeing about who owns a
    /// name is usually a deployment bug.
    #[default]
    Error,
    /// A later source replaces an earlier one, keeping the first position.
    Overwrite,
}

/// The read-side view of a tool collection.
pub trait Catalog: Send + Sync {
    /// The tool registered under `name`.
    fn get(&self, name: &str) -> Option<Arc<dyn Tool>>;

    /// Every tool definition, sorted by name, as one immutable snapshot.
    ///
    /// Returning a shared slice rather than a fresh `Vec` is what makes the
    /// per-round read paths allocation-free: the visibility computation and the
    /// `tool_search` index both derive from this value, and both would otherwise
    /// copy every definition (name + description + schema) on every call.
    ///
    /// A catalog whose contents are stable must return the **same** `Arc` for
    /// the same contents: callers use [`Arc::ptr_eq`] as their cache key. A
    /// freshly allocated slice per call is still correct, it just forgoes the
    /// reuse. An invariant worth stating because it is easy to break: a
    /// `Tool::definition` must be stable between two calls that return the same
    /// snapshot (a lazy tool that finishes loading is a *change*, and must
    /// produce a new snapshot — see `LazyProxy::ensure_loaded`).
    fn definitions(&self) -> Arc<[msg::ToolDefinition]>;
}

/// A deferred loader: builds the real tool on first use.
pub type Loader = Arc<dyn Fn() -> BoxFuture<'static, Result<Arc<dyn Tool>>> + Send + Sync>;

/// A loaded-on-first-use tool contributed by a source.
pub struct LazyTool {
    /// The registry key; must equal `placeholder.name`.
    pub name: String,
    /// The definition served until the first load.
    pub placeholder: msg::ToolDefinition,
    /// The loader.
    pub load: Loader,
}

impl std::fmt::Debug for LazyTool {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("LazyTool")
            .field("name", &self.name)
            .finish_non_exhaustive()
    }
}

impl LazyTool {
    /// Build a descriptor, checking its invariants.
    pub fn new(
        name: impl Into<String>,
        placeholder: msg::ToolDefinition,
        load: Loader,
    ) -> Result<Self> {
        let name = name.into();
        if name.trim().is_empty() {
            return Err(Error::validation("tool: lazy tool name is required"));
        }
        if placeholder.name != name {
            return Err(Error::validation(format!(
                "tool: lazy tool {name:?} placeholder must declare the same name"
            )));
        }
        Ok(Self {
            name,
            placeholder,
            load,
        })
    }
}

/// The runtime mutation surface a source uses to publish late discoveries.
pub trait Registrar: Send + Sync {
    /// Register one tool. Duplicates follow the registry's policy; a closed
    /// registry refuses the add.
    fn add(&self, tool: Arc<dyn Tool>) -> Result<()>;

    /// Unregister the named tool, if present.
    fn remove(&self, name: &str);
}

/// A multi-tool contributor to a [`Registry`].
#[async_trait]
pub trait Source: Send + Sync {
    /// Tools available immediately.
    async fn tools(&self) -> Result<Vec<Arc<dyn Tool>>>;

    /// Tools that load on first use. Sources that connect in the background
    /// publish them here so a turn never waits on network I/O.
    fn lazy_tools(&self) -> Vec<LazyTool> {
        Vec::new()
    }

    /// Receive the registry's [`Registrar`], for tools discovered after
    /// construction.
    fn attach(&self, _registrar: Arc<dyn Registrar>) {}

    /// The registry closed; drop the registrar and stop background work.
    fn detach(&self) {}
}

/// A source over a fixed list of tools.
#[derive(Default)]
pub struct StaticSource {
    tools: Vec<Arc<dyn Tool>>,
    lazy: Vec<LazyTool>,
}

impl StaticSource {
    /// An empty source.
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    /// Add one eager tool.
    pub fn push(&mut self, tool: Arc<dyn Tool>) {
        self.tools.push(tool);
    }

    /// Add one deferred tool.
    pub fn push_lazy(&mut self, tool: LazyTool) {
        self.lazy.push(tool);
    }
}

#[async_trait]
impl Source for StaticSource {
    async fn tools(&self) -> Result<Vec<Arc<dyn Tool>>> {
        Ok(self.tools.clone())
    }

    fn lazy_tools(&self) -> Vec<LazyTool> {
        // LazyTool is not Clone; descriptors are rebuilt from the same fields.
        self.lazy
            .iter()
            .map(|tool| LazyTool {
                name: tool.name.clone(),
                placeholder: tool.placeholder.clone(),
                load: tool.load.clone(),
            })
            .collect()
    }
}

#[derive(Default)]
struct Inner {
    tools: BTreeMap<String, Arc<dyn Tool>>,
    order: Vec<String>,
    closed: bool,
    /// Bumped by every change to what `definitions` would return: `add`,
    /// `remove`, and a lazy tool finishing its first load (a change even though
    /// nothing was registered). Under the write lock, so the value a snapshot is
    /// built at cannot move while it is being built.
    revisions: u64,
}

impl Inner {
    /// Announce that the definitions may have changed.
    fn touch(&mut self) {
        self.revisions = self.revisions.wrapping_add(1);
    }
}

/// Aggregates tools from many sources.
///
/// The eager set is fixed at construction; runtime mutation is through the
/// [`Registrar`] handed to sources (and [`Registry::add`]).
pub struct Registry {
    inner: Arc<RwLock<Inner>>,
    conflict: ConflictPolicy,
    sources: Vec<Arc<dyn Source>>,
    /// The definitions built at `Inner::revisions`, kept until the next change.
    snapshot: Mutex<Option<(u64, Arc<[msg::ToolDefinition]>)>>,
}

impl std::fmt::Debug for Registry {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Registry")
            .field("len", &self.len())
            .field("conflict", &self.conflict)
            .finish()
    }
}

impl Registry {
    fn bare(conflict: ConflictPolicy, sources: Vec<Arc<dyn Source>>) -> Self {
        Self {
            inner: Arc::new(RwLock::new(Inner::default())),
            conflict,
            sources,
            snapshot: Mutex::new(None),
        }
    }

    /// An empty registry.
    #[must_use]
    pub fn empty() -> Arc<Self> {
        Arc::new(Self::bare(ConflictPolicy::Error, Vec::new()))
    }

    /// Build a registry from sources in order, then hand each source its
    /// registrar.
    pub async fn from_sources(
        sources: &[Arc<dyn Source>],
        conflict: ConflictPolicy,
    ) -> Result<Arc<Self>> {
        let registry = Arc::new(Self::bare(conflict, sources.to_vec()));
        for source in sources {
            for tool in source.tools().await? {
                registry.add(tool)?;
            }
            for lazy in source.lazy_tools() {
                registry.add(Arc::new(LazyProxy::new(
                    lazy,
                    Arc::downgrade(&registry.inner),
                )))?;
            }
        }
        let registrar = registry.registrar();
        for source in sources {
            source.attach(registrar.clone());
        }
        Ok(registry)
    }

    /// A registrar for this registry. Safe to hold from a source: it only keeps
    /// a weak reference to the registry.
    #[must_use]
    pub fn registrar(&self) -> Arc<dyn Registrar> {
        Arc::new(RegistrarHandle {
            inner: Arc::downgrade(&self.inner),
            conflict: self.conflict,
        })
    }

    /// Register one tool at runtime.
    pub fn add(&self, tool: Arc<dyn Tool>) -> Result<()> {
        let mut inner = self.write()?;
        add(&mut inner, self.conflict, tool)
    }

    /// Unregister a tool. Unknown names are ignored.
    pub fn remove(&self, name: &str) {
        if let Ok(mut inner) = self.inner.write()
            && inner.tools.remove(name).is_some()
        {
            inner.order.retain(|existing| existing != name);
            inner.touch();
        }
    }

    /// The registered tool names, sorted.
    #[must_use]
    pub fn names(&self) -> Vec<String> {
        // `tools` is a `BTreeMap`, so its keys are already sorted: no `order`
        // copy and no sort.
        match self.inner.read() {
            Ok(inner) => inner.tools.keys().cloned().collect(),
            Err(_) => Vec::new(),
        }
    }

    /// Number of registered tools.
    #[must_use]
    pub fn len(&self) -> usize {
        self.inner
            .read()
            .map(|inner| inner.tools.len())
            .unwrap_or(0)
    }

    /// Whether the registry is empty.
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }

    /// Close the registry: no further adds, every tool's [`Tool::close`] runs in
    /// reverse registration order, and sources are detached.
    pub fn close(&self) -> Result<()> {
        let order = {
            let mut inner = self.write()?;
            if inner.closed {
                return Ok(());
            }
            inner.closed = true;
            inner.order.clone()
        };
        let mut first: Option<Error> = None;
        for name in order.iter().rev() {
            if let Some(tool) = self.get(name)
                && let Err(error) = tool.close()
                && first.is_none()
            {
                first = Some(error);
            }
        }
        for source in &self.sources {
            source.detach();
        }
        match first {
            Some(error) => Err(error),
            None => Ok(()),
        }
    }

    fn write(&self) -> Result<std::sync::RwLockWriteGuard<'_, Inner>> {
        self.inner
            .write()
            .map_err(|_| Error::internal("tool registry lock poisoned"))
    }
}

fn add(inner: &mut Inner, conflict: ConflictPolicy, tool: Arc<dyn Tool>) -> Result<()> {
    if inner.closed {
        return Err(Error::not_available("tool: registry is closed"));
    }
    let name = tool.definition().name;
    if name.trim().is_empty() {
        return Err(Error::validation("tool: definition name is required"));
    }
    match inner.tools.entry(name) {
        std::collections::btree_map::Entry::Occupied(mut entry) => match conflict {
            ConflictPolicy::Error => Err(Error::conflict(format!(
                "tool: duplicate tool {:?} from multiple sources",
                entry.key()
            ))),
            ConflictPolicy::Overwrite => {
                entry.insert(tool);
                inner.touch();
                Ok(())
            }
        },
        std::collections::btree_map::Entry::Vacant(entry) => {
            inner.order.push(entry.key().clone());
            entry.insert(tool);
            inner.touch();
            Ok(())
        }
    }
}

impl Catalog for Registry {
    fn get(&self, name: &str) -> Option<Arc<dyn Tool>> {
        self.inner.read().ok()?.tools.get(name).cloned()
    }

    fn definitions(&self) -> Arc<[msg::ToolDefinition]> {
        // One read guard covers both the revision and the tools it describes, so
        // a writer cannot slip between the two. `tools` is a `BTreeMap`, so
        // `values()` is already sorted by name.
        let Ok(inner) = self.inner.read() else {
            return Vec::new().into();
        };
        let revision = inner.revisions;
        if let Ok(cache) = self.snapshot.lock()
            && let Some((at, snapshot)) = cache.as_ref()
            && *at == revision
        {
            return Arc::clone(snapshot);
        }
        // Built under the read guard: a mutation cannot interleave, so the
        // revision published below describes exactly these definitions.
        let built: Arc<[msg::ToolDefinition]> =
            inner.tools.values().map(|tool| tool.definition()).collect();
        if let Ok(mut cache) = self.snapshot.lock() {
            *cache = Some((revision, Arc::clone(&built)));
        }
        built
    }
}

struct RegistrarHandle {
    inner: Weak<RwLock<Inner>>,
    conflict: ConflictPolicy,
}

impl Registrar for RegistrarHandle {
    fn add(&self, tool: Arc<dyn Tool>) -> Result<()> {
        let Some(inner) = self.inner.upgrade() else {
            return Err(Error::not_available("tool: registry is gone"));
        };
        let mut inner = inner
            .write()
            .map_err(|_| Error::internal("tool registry lock poisoned"))?;
        add(&mut inner, self.conflict, tool)
    }

    fn remove(&self, name: &str) {
        let Some(inner) = self.inner.upgrade() else {
            return;
        };
        if let Ok(mut inner) = inner.write()
            && inner.tools.remove(name).is_some()
        {
            inner.order.retain(|existing| existing != name);
            inner.touch();
        }
    }
}

/// The registry entry for a [`LazyTool`]: serves the placeholder until the
/// first load, then the real tool.
///
/// The upgrade is a *definition change*, not a registration: without telling the
/// registry, a cached snapshot would serve the placeholder forever (the tool is
/// loaded, the model still sees “deferred”). Hence the back-pointer to `Inner`.
struct LazyProxy {
    spec: LazyTool,
    loaded: OnceLock<Arc<dyn Tool>>,
    guard: tokio::sync::Mutex<()>,
    closed: AtomicBool,
    registry: Weak<RwLock<Inner>>,
}

impl LazyProxy {
    fn new(spec: LazyTool, registry: Weak<RwLock<Inner>>) -> Self {
        Self {
            spec,
            loaded: OnceLock::new(),
            guard: tokio::sync::Mutex::new(()),
            closed: AtomicBool::new(false),
            registry,
        }
    }

    /// Tell the registry that this tool's definition changed.
    ///
    /// Happens under the write lock (the same one `add`/`remove` take), so a
    /// snapshot being built right now either already includes the real
    /// definition or will be rebuilt next time.
    fn announce(&self) {
        let Some(registry) = self.registry.upgrade() else {
            return;
        };
        if let Ok(mut inner) = registry.write() {
            inner.touch();
        }
    }
}

#[async_trait]
impl Tool for LazyProxy {
    fn definition(&self) -> msg::ToolDefinition {
        self.loaded
            .get()
            .map_or_else(|| self.spec.placeholder.clone(), |tool| tool.definition())
    }

    async fn execute(
        &self,
        context: &crate::caps::tool::callable::CallContext,
        arguments: &str,
    ) -> Result<msg::Content> {
        self.ensure_loaded().await?;
        let tool = self.loaded.get().ok_or_else(|| {
            Error::not_available(format!("tool {:?}: not loaded", self.spec.name))
        })?;
        tool.execute(context, arguments).await
    }

    fn metadata(&self) -> ToolMeta {
        self.loaded
            .get()
            .map(|tool| tool.metadata())
            .unwrap_or_default()
    }

    fn close(&self) -> Result<()> {
        self.closed.store(true, Ordering::SeqCst);
        match self.loaded.get() {
            Some(tool) => tool.close(),
            None => Ok(()),
        }
    }

    async fn ensure_loaded(&self) -> Result<()> {
        if self.loaded.get().is_some() {
            return Ok(());
        }
        if self.closed.load(Ordering::SeqCst) {
            return Err(Error::not_available(format!(
                "tool {:?}: registry is closed",
                self.spec.name
            )));
        }
        let _guard = self.guard.lock().await;
        if self.loaded.get().is_some() {
            return Ok(());
        }
        let tool = (self.spec.load)().await?;
        let _ = self.loaded.set(tool);
        // The placeholder is no longer what `definition()` returns: a cached
        // snapshot would be wrong from here on.
        self.announce();
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Mutex;

    use crate::caps::tool::callable::FuncTool;
    use crate::error::ErrorKind;
    use crate::message::text_content;

    fn definition(name: &str, description: &str) -> msg::ToolDefinition {
        msg::ToolDefinition {
            name: name.to_owned(),
            description: description.to_owned(),
            input_schema: Vec::new(),
        }
    }

    fn tool(name: &str) -> Arc<dyn Tool> {
        Arc::new(FuncTool::new(
            definition(name, ""),
            Arc::new(|_context, _| Ok(text_content("ok"))),
        ))
    }

    #[tokio::test]
    async fn sources_merge_in_order_and_sort_definitions() {
        let mut first = StaticSource::new();
        first.push(tool("b"));
        let mut second = StaticSource::new();
        second.push(tool("a"));
        let registry =
            Registry::from_sources(&[Arc::new(first), Arc::new(second)], ConflictPolicy::Error)
                .await
                .expect("build");
        assert_eq!(registry.names(), vec!["a", "b"]);
        assert_eq!(
            registry
                .definitions()
                .iter()
                .map(|d| d.name.as_str())
                .collect::<Vec<_>>(),
            vec!["a", "b"]
        );
    }

    #[tokio::test]
    async fn a_duplicate_is_a_conflict_by_default() {
        let mut first = StaticSource::new();
        first.push(tool("same"));
        let mut second = StaticSource::new();
        second.push(tool("same"));
        let error =
            Registry::from_sources(&[Arc::new(first), Arc::new(second)], ConflictPolicy::Error)
                .await
                .unwrap_err();
        assert_eq!(error.kind(), ErrorKind::Conflict);
    }

    #[tokio::test]
    async fn overwrite_lets_the_later_source_win() {
        let mut first = StaticSource::new();
        first.push(Arc::new(FuncTool::new(
            definition("same", "first"),
            Arc::new(|_context, _| Ok(text_content("first"))),
        )));
        let mut second = StaticSource::new();
        second.push(Arc::new(FuncTool::new(
            definition("same", "second"),
            Arc::new(|_context, _| Ok(text_content("second"))),
        )));
        let registry = Registry::from_sources(
            &[Arc::new(first), Arc::new(second)],
            ConflictPolicy::Overwrite,
        )
        .await
        .expect("build");
        assert_eq!(registry.len(), 1);
        assert_eq!(registry.definitions()[0].description, "second");
    }

    #[tokio::test]
    async fn add_remove_and_close() {
        let registry = Registry::empty();
        registry.add(tool("a")).expect("add");
        registry.remove("a");
        assert!(registry.is_empty());
        registry.close().expect("close");
        let error = registry.add(tool("b")).unwrap_err();
        assert_eq!(error.kind(), ErrorKind::NotAvailable);
    }

    #[tokio::test]
    async fn close_runs_in_reverse_order() {
        #[derive(Clone)]
        struct Closer {
            name: String,
            calls: Arc<Mutex<Vec<String>>>,
        }

        #[async_trait]
        impl Tool for Closer {
            fn definition(&self) -> msg::ToolDefinition {
                definition(&self.name, "")
            }

            async fn execute(
                &self,
                _context: &crate::caps::tool::callable::CallContext,
                _arguments: &str,
            ) -> Result<msg::Content> {
                Ok(text_content("ok"))
            }

            fn close(&self) -> Result<()> {
                if let Ok(mut calls) = self.calls.lock() {
                    calls.push(self.name.clone());
                }
                Ok(())
            }
        }

        let calls = Arc::new(Mutex::new(Vec::new()));
        let mut source = StaticSource::new();
        for name in ["a", "b"] {
            source.push(Arc::new(Closer {
                name: name.to_owned(),
                calls: calls.clone(),
            }));
        }
        let registry = Registry::from_sources(&[Arc::new(source)], ConflictPolicy::Error)
            .await
            .expect("build");
        registry.close().expect("close");
        assert_eq!(*calls.lock().expect("lock"), vec!["b", "a"]);
    }

    /// The snapshot cache is keyed by `Inner::revisions`, and a lazy load is a
    /// change that registers nothing. Without the announcement the placeholder
    /// would be served forever — the loaded tool would be invisible to the model.
    #[tokio::test]
    async fn a_loaded_lazy_tool_replaces_its_placeholder_in_the_snapshot() {
        let mut source = StaticSource::new();
        source.push_lazy(
            LazyTool::new(
                "lazy",
                definition("lazy", "placeholder"),
                Arc::new(|| {
                    Box::pin(async {
                        Ok(Arc::new(FuncTool::new(
                            definition("lazy", "real"),
                            Arc::new(|_context, _| Ok(text_content("ok"))),
                        )) as Arc<dyn Tool>)
                    })
                }),
            )
            .expect("lazy"),
        );
        let registry = Registry::from_sources(&[Arc::new(source)], ConflictPolicy::Error)
            .await
            .expect("build");

        // Prime the cache with the placeholder.
        let before = registry.definitions();
        assert_eq!(before[0].description, "placeholder");
        assert!(Arc::ptr_eq(&before, &registry.definitions()));

        registry
            .get("lazy")
            .expect("registered")
            .ensure_loaded()
            .await
            .expect("load");

        let after = registry.definitions();
        assert_eq!(after[0].description, "real");
        assert!(!Arc::ptr_eq(&before, &after));
    }

    #[tokio::test]
    async fn a_lazy_tool_loads_once_on_first_use() {
        let loads = Arc::new(std::sync::atomic::AtomicUsize::new(0));
        let counter = loads.clone();
        let lazy = LazyTool::new(
            "lazy",
            definition("lazy", "placeholder"),
            Arc::new(move || {
                counter.fetch_add(1, Ordering::SeqCst);
                Box::pin(async { Ok(tool("lazy")) })
            }),
        )
        .expect("lazy");
        let mut source = StaticSource::new();
        source.push_lazy(lazy);
        let registry = Registry::from_sources(&[Arc::new(source)], ConflictPolicy::Error)
            .await
            .expect("build");
        let lazy = registry.get("lazy").expect("registered");
        assert_eq!(lazy.definition().description, "placeholder");
        lazy.execute(&crate::caps::tool::callable::CallContext::default(), "{}")
            .await
            .expect("execute");
        lazy.execute(&crate::caps::tool::callable::CallContext::default(), "{}")
            .await
            .expect("execute");
        assert_eq!(loads.load(Ordering::SeqCst), 1);
    }

    #[tokio::test]
    async fn an_attached_source_publishes_tools_after_construction() {
        struct Late {
            registrar: Mutex<Option<Arc<dyn Registrar>>>,
        }

        #[async_trait]
        impl Source for Late {
            async fn tools(&self) -> Result<Vec<Arc<dyn Tool>>> {
                Ok(Vec::new())
            }

            fn attach(&self, registrar: Arc<dyn Registrar>) {
                if let Ok(mut slot) = self.registrar.lock() {
                    *slot = Some(registrar);
                }
            }
        }

        let late = Arc::new(Late {
            registrar: Mutex::new(None),
        });
        let source: Arc<dyn Source> = late.clone();
        let registry = Registry::from_sources(std::slice::from_ref(&source), ConflictPolicy::Error)
            .await
            .expect("build");
        let registrar = late
            .registrar
            .lock()
            .expect("lock")
            .clone()
            .expect("attached");
        registrar.add(tool("late")).expect("add");
        assert!(registry.get("late").is_some());
        registry.close().expect("close");
        // The registrar is weak: it cannot resurrect a closed registry.
        assert_eq!(
            registrar.add(tool("after")).unwrap_err().kind(),
            ErrorKind::NotAvailable
        );
    }
}
