//! Construction, lifecycle, and the built graph.

use std::any::Any;
use std::collections::BTreeMap;
use std::path::PathBuf;
use std::sync::Arc;

use crate::error::{Error, Result};
use crate::logerr;
use crate::resource::Deployment;

use crate::resource::plan::{Plan, PlanError, plan, render_errors};
use crate::resource::registry::Registry;
use crate::resource::resolver::Resolver;
use crate::resource::spec::{BoxFuture, LifecycleFn, ready};
use crate::resource::{BuildCtx, ResolvedDeps, Value};

/// Called after each successful construction, with the resource name and value.
///
/// The kernel itself does not know what a value means; this is how a host
/// attaches cross-cutting state (the deployment secret store, for example)
/// without the resource crate depending on any capability.
pub type BuildObserver = Arc<dyn Fn(&str, &Value) + Send + Sync>;

/// The lifecycle of a built value.
///
/// Unlike the Go side's optional `Startable` assertion, every built value
/// implements this trait; both methods default to a no-op. The kernel can then
/// drive start/shutdown without guessing, which removes the "forgot to forward
/// the capability through a decorator" class of bug.
pub trait Resource: Any + Send + Sync + 'static {
    /// Start background work (a listener, a poller). Called after every value
    /// has been constructed, in construction order.
    fn start(self: Arc<Self>) -> BoxFuture<'static, Result<()>> {
        ready(Ok(()))
    }

    /// Release everything. Called in reverse construction order. Must be
    /// idempotent: a failed build shuts down what it built, then the caller may
    /// shut the rest down again.
    fn shutdown(self: Arc<Self>) -> BoxFuture<'static, Result<()>> {
        ready(Ok(()))
    }
}

/// A value the host injects before planning.
///
/// Externals are leaves: never constructed, never shut down, same process only.
/// They carry their concrete type name so diagnostics can name it without
/// reflection.
#[derive(Clone)]
pub struct ExternalResource {
    /// The dependency name.
    pub name: String,
    /// The contract the value satisfies.
    pub contract: String,
    /// The value.
    pub value: Value,
    /// The concrete type name, for diagnostics.
    pub type_name: &'static str,
}

impl ExternalResource {
    /// Wrap a typed host value.
    #[must_use]
    pub fn new<T: Any + Send + Sync>(
        name: impl Into<String>,
        contract: impl Into<String>,
        value: Arc<T>,
    ) -> Self {
        Self {
            name: name.into(),
            contract: contract.into(),
            value: value as Value,
            type_name: std::any::type_name::<T>(),
        }
    }

    /// Wrap an already-erased value, supplying the type name for diagnostics.
    #[must_use]
    pub fn from_erased(
        name: impl Into<String>,
        contract: impl Into<String>,
        value: Value,
        type_name: &'static str,
    ) -> Self {
        Self {
            name: name.into(),
            contract: contract.into(),
            value,
            type_name,
        }
    }

    /// Checks the declaration.
    pub fn validate(&self) -> Result<()> {
        crate::resource::validate_name("external", &self.name)?;
        if self.contract.trim().is_empty() {
            return Err(Error::validation(format!(
                "external {:?}: contract is empty",
                self.name
            )));
        }
        Ok(())
    }
}

/// Plans and constructs one deployment.
pub struct Builder {
    registry: Arc<Registry>,
    externals: Vec<ExternalResource>,
    resolver: Option<Resolver>,
    base_dir: Option<PathBuf>,
    observer: Option<BuildObserver>,
}

impl Builder {
    /// A builder over a registry.
    #[must_use]
    pub fn new(registry: Arc<Registry>) -> Self {
        Self {
            registry,
            externals: Vec::new(),
            resolver: None,
            base_dir: None,
            observer: None,
        }
    }

    /// Install a post-construction observer.
    #[must_use]
    pub fn with_observer(mut self, observer: BuildObserver) -> Self {
        self.observer = Some(observer);
        self
    }

    /// Add injected externals.
    #[must_use]
    pub fn with_externals(mut self, externals: impl IntoIterator<Item = ExternalResource>) -> Self {
        self.externals.extend(externals);
        self
    }

    /// Add one injected external.
    #[must_use]
    pub fn with_external(mut self, external: ExternalResource) -> Self {
        self.externals.push(external);
        self
    }

    /// Install the reference resolver used for `${...}` expansion.
    #[must_use]
    pub fn with_resolver(mut self, resolver: Resolver) -> Self {
        self.resolver = Some(resolver);
        self
    }

    /// Set the document loader's base directory.
    #[must_use]
    pub fn with_base_dir(mut self, dir: impl Into<PathBuf>) -> Self {
        self.base_dir = Some(dir.into());
        self
    }

    /// The pure check phase. No I/O, nothing constructed.
    pub fn plan(&self, deployment: Deployment) -> std::result::Result<Plan, Vec<PlanError>> {
        plan(&self.registry, &self.externals, deployment)
    }

    /// Plan, then construct, then (on failure) shut down what was built.
    pub async fn deploy(&self, deployment: Deployment) -> Result<Built> {
        let plan = self
            .plan(deployment)
            .map_err(|errors| render_errors(&errors))?;
        tracing::debug!(flowcraft.resources = plan.order.len(), "deploying");
        self.build(plan).await
    }

    async fn build(&self, plan: Plan) -> Result<Built> {
        let mut values: BTreeMap<String, Value> = plan
            .externals
            .iter()
            .map(|(name, external)| (name.clone(), external.value.clone()))
            .collect();
        let mut lifecycle: BTreeMap<String, (LifecycleFn, LifecycleFn)> = BTreeMap::new();
        let mut built: Vec<String> = Vec::new();

        for name in &plan.order {
            let Some(node) = plan.nodes.get(name) else {
                return Err(Error::internal(format!(
                    "plan order names an unknown resource {name:?}"
                )));
            };

            let mut deps = ResolvedDeps::default();
            for edge in &node.edges {
                let Some(value) = values.get(&edge.producer).cloned() else {
                    let error =
                        Error::internal(format!("dependency {:?} was never built", edge.producer));
                    shutdown_all(&built, &values, &lifecycle).await;
                    return Err(error);
                };
                if edge.many {
                    deps.push_many(edge.requirement, edge.name.clone(), value);
                } else {
                    deps.push_single(edge.name.clone(), value);
                }
            }

            let settings = match (&node.settings, &self.resolver) {
                (Some(settings), Some(resolver)) => match resolver.expand(settings).await {
                    Ok(expanded) => Some(expanded),
                    Err(error) => {
                        shutdown_all(&built, &values, &lifecycle).await;
                        return Err(error);
                    }
                },
                (Some(settings), None) => Some(settings.clone()),
                (None, _) => None,
            };

            let ctx = BuildCtx {
                name,
                settings: settings.as_ref(),
                deps: &deps,
                base_dir: self.base_dir.as_deref(),
                secrets: self
                    .resolver
                    .as_ref()
                    .and_then(|resolver| resolver.table())
                    .map(|table| &**table),
            };

            match node.factory.build(ctx).await {
                Ok(value) => {
                    if let Some(observer) = &self.observer {
                        observer(name, &value);
                    }
                    values.insert(name.clone(), value);
                    lifecycle.insert(
                        name.clone(),
                        (node.factory.start(), node.factory.shutdown()),
                    );
                    built.push(name.clone());
                    // One line per resource, at debug: a deployment can have
                    // dozens, and the alternative is a boot that explains
                    // nothing when it stalls.
                    tracing::debug!(flowcraft.resource = %name, "resource built");
                }
                Err(error) => {
                    shutdown_all(&built, &values, &lifecycle).await;
                    return Err(error);
                }
            }
        }

        Ok(Built {
            entries: plan.deployment.entries.clone(),
            values,
            order: built,
            lifecycle,
        })
    }
}

/// Best-effort teardown of a partially built graph.
///
/// The build error is the one worth returning, but a failure to tear down is
/// still a leaked resource: it is reported rather than dropped.
async fn shutdown_all(
    built: &[String],
    values: &BTreeMap<String, Value>,
    lifecycle: &BTreeMap<String, (LifecycleFn, LifecycleFn)>,
) {
    for name in built.iter().rev() {
        if let (Some((_, shutdown)), Some(value)) = (lifecycle.get(name), values.get(name))
            && let Err(failure) = shutdown(value.clone()).await
        {
            logerr!(
                warn,
                failure,
                "resource shutdown failed while rolling back a build",
                flowcraft.resource = %name
            );
        }
    }
}

/// A constructed graph.
pub struct Built {
    values: BTreeMap<String, Value>,
    order: Vec<String>,
    lifecycle: BTreeMap<String, (LifecycleFn, LifecycleFn)>,
    entries: Vec<String>,
}

impl std::fmt::Debug for Built {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Built")
            .field("order", &self.order)
            .field("entries", &self.entries)
            .finish_non_exhaustive()
    }
}

impl Built {
    /// Read a built value, downcasting to `T`.
    pub fn get<T: Any + Send + Sync>(&self, name: &str) -> Result<Arc<T>> {
        let value = self
            .values
            .get(name)
            .ok_or_else(|| Error::not_found(format!("resource {name:?} is not built")))?;
        value.clone().downcast::<T>().map_err(|_| {
            Error::validation(format!(
                "resource {name:?} is not a {}",
                std::any::type_name::<T>()
            ))
        })
    }

    /// Read a built value without narrowing.
    #[must_use]
    pub fn raw(&self, name: &str) -> Option<&Value> {
        self.values.get(name)
    }

    /// Every built resource name, in construction order.
    #[must_use]
    pub fn names(&self) -> &[String] {
        &self.order
    }

    /// The document's `entries`, or every resource when it names none.
    #[must_use]
    pub fn entries(&self) -> Vec<(String, Value)> {
        let names: Vec<String> = if self.entries.is_empty() {
            self.order.clone()
        } else {
            self.entries.clone()
        };
        names
            .into_iter()
            .filter_map(|name| self.values.get(&name).cloned().map(|value| (name, value)))
            .collect()
    }

    /// Start every resource, in construction order. On failure, shuts down the
    /// ones already started.
    pub async fn start(&self) -> Result<()> {
        let mut started: Vec<&String> = Vec::new();
        for name in &self.order {
            let Some((start, _)) = self.lifecycle.get(name) else {
                continue;
            };
            let Some(value) = self.values.get(name) else {
                continue;
            };
            if let Err(error) = start(value.clone()).await {
                for done in started.iter().rev() {
                    if let (Some((_, shutdown)), Some(value)) =
                        (self.lifecycle.get(*done), self.values.get(*done))
                        && let Err(failure) = shutdown(value.clone()).await
                    {
                        logerr!(
                            warn,
                            failure,
                            "resource shutdown failed while rolling back a start",
                            flowcraft.resource = %**done
                        );
                    }
                }
                return Err(error);
            }
            tracing::debug!(flowcraft.resource = %name, "resource started");
            started.push(name);
        }
        Ok(())
    }

    /// Shut every resource down, in reverse construction order. Continues after
    /// a failure and returns the first error.
    pub async fn shutdown(&self) -> Result<()> {
        let mut first: Option<Error> = None;
        for name in self.order.iter().rev() {
            let Some((_, shutdown)) = self.lifecycle.get(name) else {
                continue;
            };
            let Some(value) = self.values.get(name) else {
                continue;
            };
            if let Err(error) = shutdown(value.clone()).await {
                // Name the resource: a bare "operation was canceled" from one of
                // several closes is not actionable. Every failure is reported;
                // the first is also returned, because one error per call is the
                // contract.
                logerr!(
                    warn,
                    error,
                    "resource shutdown failed",
                    flowcraft.resource = %name
                );
                if first.is_none() {
                    first = Some(crate::error::Error::new(
                        error.kind(),
                        format!("{name}: {error}"),
                    ));
                }
            }
        }
        match first {
            Some(error) => Err(error),
            None => Ok(()),
        }
    }
}
