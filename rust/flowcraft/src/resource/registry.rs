//! The kind/impl registry.

use std::collections::BTreeMap;
use std::sync::Arc;

use crate::error::{Error, Result};
use crate::resource::Kind;

use crate::resource::spec::{ErasedFactory, Factory, Spec};

/// The registry identity of a factory.
#[derive(Debug, Clone, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct Key {
    /// The resource type.
    pub kind: Kind,
    /// The implementation name, absent when the kind has one implementation.
    pub imp: Option<String>,
}

impl Key {
    /// Build a key.
    #[must_use]
    pub fn new(kind: Kind, imp: Option<&str>) -> Self {
        Self {
            kind,
            imp: imp.map(str::to_owned),
        }
    }
}

impl std::fmt::Display for Key {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match &self.imp {
            Some(imp) if !imp.is_empty() => write!(f, "{}/{}", self.kind, imp),
            _ => write!(f, "{}", self.kind),
        }
    }
}

/// Explicit, in-memory wiring.
///
/// There is deliberately no distributed auto-registration (`inventory`,
/// `linkme`): the kernel has a fixed startup order and a registry that code
/// chooses, not one the linker chooses.
#[derive(Default, Clone)]
pub struct Registry {
    entries: BTreeMap<Key, Arc<dyn ErasedFactory>>,
}

impl Registry {
    /// An empty registry.
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    /// Register a factory. A duplicate `(kind, impl)` is a conflict.
    pub fn register<F: Factory + 'static>(&mut self, factory: F) -> Result<()> {
        let spec = factory.spec();
        let key = Key::new(spec.kind.clone(), spec.imp);
        if self.entries.contains_key(&key) {
            return Err(Error::conflict(format!(
                "resource factory {key} is registered twice"
            )));
        }
        self.entries.insert(key, Arc::new(factory));
        Ok(())
    }

    /// Look up a factory by document identity.
    #[must_use]
    pub fn get(&self, kind: &Kind, imp: Option<&str>) -> Option<Arc<dyn ErasedFactory>> {
        let key = Key::new(kind.clone(), imp);
        self.entries.get(&key).cloned()
    }

    /// Every factory, in `(kind, impl)` order.
    #[must_use]
    pub fn specs(&self) -> Vec<Spec> {
        self.entries
            .values()
            .map(|factory| factory.spec())
            .collect()
    }

    /// Every registry key, in order.
    #[must_use]
    pub fn keys(&self) -> Vec<Key> {
        self.entries.keys().cloned().collect()
    }

    /// Number of registered factories.
    #[must_use]
    pub fn len(&self) -> usize {
        self.entries.len()
    }

    /// Whether the registry is empty.
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.entries.is_empty()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::resource::lifecycle::Resource;
    use crate::resource::spec::BoxFuture;
    use crate::resource::{BuildCtx, Value as BuiltValue};

    struct Unit;

    impl Resource for Unit {}

    struct UnitFactory(&'static str);

    impl Factory for UnitFactory {
        type Output = Unit;

        fn spec(&self) -> Spec {
            Spec::new(Kind::new(self.0).unwrap())
        }

        fn build<'a>(&'a self, _ctx: BuildCtx<'a>) -> BoxFuture<'a, Result<Arc<Self::Output>>> {
            Box::pin(async { Ok(Arc::new(Unit)) })
        }
    }

    #[test]
    fn duplicate_registration_is_a_conflict() {
        let mut registry = Registry::new();
        registry.register(UnitFactory("test.Unit")).unwrap();
        let error = registry.register(UnitFactory("test.Unit")).unwrap_err();
        assert_eq!(error.kind(), crate::error::ErrorKind::Conflict);
    }

    #[test]
    fn lookup_is_exact_on_kind_and_impl() {
        let mut registry = Registry::new();
        registry.register(UnitFactory("test.Unit")).unwrap();
        let kind = Kind::new("test.Unit").unwrap();
        assert!(registry.get(&kind, None).is_some());
        assert!(registry.get(&kind, Some("other")).is_none());
    }

    #[test]
    fn erased_build_returns_a_downcastable_value() {
        let mut registry = Registry::new();
        registry.register(UnitFactory("test.Unit")).unwrap();
        let factory = registry
            .get(&Kind::new("test.Unit").unwrap(), None)
            .unwrap();
        let deps = crate::resource::ResolvedDeps::default();
        let ctx = BuildCtx {
            name: "unit",
            settings: None,
            deps: &deps,
            base_dir: None,
            secrets: None,
        };
        let value: BuiltValue = futures_lite_block(factory.build(ctx)).expect("build");
        assert!(value.downcast::<Unit>().is_ok());
    }

    /// The kernel itself is runtime-neutral; the test drives one future to
    /// completion with a minimal executor instead of pulling in a runtime.
    fn futures_lite_block<T>(future: impl std::future::Future<Output = T>) -> T {
        let mut future = std::pin::pin!(future);
        let mut cx = std::task::Context::from_waker(std::task::Waker::noop());
        loop {
            if let std::task::Poll::Ready(value) = future.as_mut().poll(&mut cx) {
                return value;
            }
        }
    }
}
