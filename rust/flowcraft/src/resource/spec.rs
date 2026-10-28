//! The factory contract.

use std::any::TypeId;
use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;

use crate::error::{Error, Result};
use crate::resource::Kind;
use serde_json::Value;

use crate::resource::dep::Requirement;
use crate::resource::lifecycle::Resource;
use crate::resource::{BuildCtx, Value as BuiltValue};

/// A boxed, `Send` future. The kernel is transport-neutral, so it does not
/// depend on a runtime crate to name this type.
pub type BoxFuture<'a, T> = Pin<Box<dyn Future<Output = T> + Send + 'a>>;

/// When a resource is constructed relative to the rest of the graph.
///
/// `Pre` values are built first and are allowed to contribute bootstrap state
/// (a secret store, for example). The kernel does not special-case any kind:
/// phase ordering replaces the Go side's `secret.Store` name check.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub enum Phase {
    /// Built before every `Main` resource.
    Pre,
    /// The default.
    Main,
}

impl Phase {
    /// The wire spelling used by the catalog.
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Pre => "pre",
            Self::Main => "main",
        }
    }
}

/// A factory's static declaration.
#[derive(Clone)]
pub struct Spec {
    /// The resource type.
    pub kind: Kind,
    /// The implementation name, when the kind has more than one.
    pub imp: Option<&'static str>,
    /// The cross-process contract identity, when the value travels a wire.
    pub contract: Option<&'static str>,
    /// The dependencies this factory declared, and therefore may read.
    pub deps: Vec<Requirement>,
    /// Construction phase.
    pub phase: Phase,
    /// Settings validation, run by `Plan`. Must be pure and perform no I/O.
    pub validate_settings: Option<fn(&Value) -> Result<()>>,
}

impl std::fmt::Debug for Spec {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Spec")
            .field("kind", &self.kind)
            .field("imp", &self.imp)
            .field("contract", &self.contract)
            .field("deps", &self.deps)
            .field("phase", &self.phase)
            .finish_non_exhaustive()
    }
}

impl Spec {
    /// A spec with no implementation name, dependencies, or phase.
    #[must_use]
    pub fn new(kind: Kind) -> Self {
        Self {
            kind,
            imp: None,
            contract: None,
            deps: Vec::new(),
            phase: Phase::Main,
            validate_settings: None,
        }
    }

    /// The `kind/impl` identity used for registry lookup.
    #[must_use]
    pub fn key(&self) -> (Kind, Option<String>) {
        (self.kind.clone(), self.imp.map(str::to_owned))
    }
}

/// A typed factory.
///
/// Implementors return their own type; the registry stores the erased form and
/// remembers `Output`'s [`TypeId`], which is what makes plan-time edge checking
/// possible.
pub trait Factory: Send + Sync {
    /// What this factory produces. Must implement [`Resource`] so the kernel can
    /// run its lifecycle.
    type Output: Resource;

    /// The static declaration.
    fn spec(&self) -> Spec;

    /// Construct one value.
    fn build<'a>(&'a self, ctx: BuildCtx<'a>) -> BoxFuture<'a, Result<Arc<Self::Output>>>;
}

/// The lifecycle operations of one built value, erased.
pub(crate) type LifecycleFn = fn(BuiltValue) -> BoxFuture<'static, Result<()>>;

/// The registry's view of a factory: typed construction behind an erasure
/// boundary.
pub trait ErasedFactory: Send + Sync {
    /// The static declaration.
    fn spec(&self) -> Spec;

    /// The concrete type produced, as `TypeId`.
    fn build_type(&self) -> TypeId;

    /// The concrete type produced, for diagnostics.
    fn build_type_name(&self) -> &'static str;

    /// Construct the value, erased.
    fn build<'a>(&'a self, ctx: BuildCtx<'a>) -> BoxFuture<'a, Result<BuiltValue>>;

    /// Start the value.
    fn start(&self) -> LifecycleFn;

    /// Shut the value down.
    fn shutdown(&self) -> LifecycleFn;
}

impl<F: Factory> ErasedFactory for F {
    fn spec(&self) -> Spec {
        Factory::spec(self)
    }

    fn build_type(&self) -> TypeId {
        TypeId::of::<F::Output>()
    }

    fn build_type_name(&self) -> &'static str {
        std::any::type_name::<F::Output>()
    }

    fn build<'a>(&'a self, ctx: BuildCtx<'a>) -> BoxFuture<'a, Result<BuiltValue>> {
        Box::pin(async move {
            let value = Factory::build(self, ctx).await?;
            Ok(value as BuiltValue)
        })
    }

    fn start(&self) -> LifecycleFn {
        start_of::<F::Output>
    }

    fn shutdown(&self) -> LifecycleFn {
        shutdown_of::<F::Output>
    }
}

fn start_of<T: Resource>(value: BuiltValue) -> BoxFuture<'static, Result<()>> {
    match value.downcast::<T>() {
        Ok(concrete) => Resource::start(concrete),
        Err(_) => Box::pin(async { Err(Error::internal("resource lifecycle downcast failed")) }),
    }
}

fn shutdown_of<T: Resource>(value: BuiltValue) -> BoxFuture<'static, Result<()>> {
    match value.downcast::<T>() {
        Ok(concrete) => Resource::shutdown(concrete),
        Err(_) => Box::pin(async { Err(Error::internal("resource lifecycle downcast failed")) }),
    }
}

/// A future that is already complete, for default lifecycle methods.
pub(crate) fn ready(value: Result<()>) -> BoxFuture<'static, Result<()>> {
    Box::pin(async move { value })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::resource::BuildCtx;
    use std::sync::Arc;

    struct Number;

    impl Resource for Number {}

    struct NumberFactory;

    impl Factory for NumberFactory {
        type Output = Number;

        fn spec(&self) -> Spec {
            Spec::new(Kind::new("test.Number").unwrap())
        }

        fn build<'a>(&'a self, _ctx: BuildCtx<'a>) -> BoxFuture<'a, Result<Arc<Self::Output>>> {
            Box::pin(async { Ok(Arc::new(Number)) })
        }
    }

    #[test]
    fn erased_factory_keeps_the_concrete_type() {
        let factory = NumberFactory;
        assert_eq!(factory.build_type(), TypeId::of::<Number>());
        assert!(factory.build_type_name().ends_with("Number"));
    }
}
