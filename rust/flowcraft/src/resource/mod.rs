//! The local composition kernel.
//!
//! A document names resources; [`Plan`] checks types and dependencies without
//! I/O; [`Builder::deploy`] constructs them in topological order; [`Built`]
//! starts them in that order and shuts them down in reverse.
//!
//! # Deliberately local
//!
//! Remote capabilities are services reached through endpoint grants
//! (`flowcraft-kernel`), not factories registered here. Nothing in this crate
//! opens a socket.
//!
//! # Typed edges
//!
//! Go checks a dependency edge by comparing interface satisfaction at plan
//! time. Rust has no such runtime check, so the edge carries two identities:
//! `TypeId` (the process-local contract) and an optional `Contract` string (the
//! cross-process one). A factory's `Output` type is recorded when it is
//! registered; `Plan` compares it to the requirement, so a mis-wired document
//! fails before a single value is constructed.
//!
//! # Lifecycle
//!
//! Every built value implements [`Resource`], whose `start` and `shutdown`
//! default to no-ops. That is stricter than the Go side's optional `Startable`
//! assertion, and on purpose: it removes the "forgot to forward `Close`" class
//! of bug that Go can only catch with tests.

#![cfg_attr(test, allow(clippy::unwrap_used, clippy::expect_used))]
pub mod catalog;
pub mod decode;
pub mod dep;
pub mod document;
pub mod layers;
pub mod lifecycle;
pub mod plan;
pub mod registry;
pub mod resolver;
pub mod secrets;
pub mod spec;

use std::any::{Any, TypeId};
use std::collections::BTreeMap;
use std::sync::Arc;

pub use self::lifecycle::{BuildObserver, Builder, Built, ExternalResource, Resource};
pub use catalog::{Catalog, CatalogDep, CatalogEntry};
pub use decode::{deployment, deployment_str, settings};
pub use dep::{Requirement, optional, optional_many, require, require_any, require_many};
pub use document::{Deployment, Deps, External, Kind, Ref, validate_name};
pub use layers::{Layer, Provenance};
pub use plan::{Plan, PlanError};
pub use registry::{Key, Registry};
pub use resolver::Resolver;
pub use secrets::{Secret, SecretRef, SecretTable};
pub use spec::{BoxFuture, Factory, Phase, Spec};

use crate::error::{Error, Result};

/// An erased built value.
pub type Value = Arc<dyn Any + Send + Sync>;

/// The build-time type of a value, without consuming it.
///
/// `value.type_id()` would name `Arc<dyn Any>`; this reaches through the
/// pointer to the concrete type.
#[must_use]
pub fn build_type_of(value: &Value) -> TypeId {
    (**value).type_id()
}

/// Builds a dependency map for a factory. Constructed by the kernel only.
#[derive(Debug, Default, Clone)]
pub struct ResolvedDeps {
    single: BTreeMap<String, Value>,
    many: BTreeMap<String, Vec<(String, Value)>>,
}

impl ResolvedDeps {
    /// Read a single dependency, downcasting to `T`.
    pub fn get<T: Any + Send + Sync>(&self, name: &str) -> Result<Arc<T>> {
        let value = self
            .single
            .get(name)
            .ok_or_else(|| Error::not_found(format!("dependency {name:?} is not resolved")))?;
        value
            .clone()
            .downcast::<T>()
            .map_err(|_| type_mismatch(name, std::any::type_name::<T>()))
    }

    /// Read an optional single dependency.
    pub fn get_optional<T: Any + Send + Sync>(&self, name: &str) -> Result<Option<Arc<T>>> {
        match self.single.get(name) {
            None => Ok(None),
            Some(value) => value
                .clone()
                .downcast::<T>()
                .map(Some)
                .map_err(|_| type_mismatch(name, std::any::type_name::<T>())),
        }
    }

    /// Read a many-dependency: edges named `name` or `name.<suffix>`, sorted by
    /// edge name so construction is deterministic.
    pub fn get_many<T: Any + Send + Sync>(&self, name: &str) -> Result<Vec<(String, Arc<T>)>> {
        let mut out = Vec::new();
        if let Some(value) = self.single.get(name) {
            out.push((
                name.to_owned(),
                value
                    .clone()
                    .downcast::<T>()
                    .map_err(|_| type_mismatch(name, std::any::type_name::<T>()))?,
            ));
        }
        if let Some(items) = self.many.get(name) {
            for (edge, value) in items {
                out.push((
                    edge.clone(),
                    value
                        .clone()
                        .downcast::<T>()
                        .map_err(|_| type_mismatch(edge, std::any::type_name::<T>()))?,
                ));
            }
        }
        out.sort_by(|left, right| left.0.cmp(&right.0));
        Ok(out)
    }

    /// The raw value under an edge name, without narrowing.
    #[must_use]
    pub fn raw(&self, name: &str) -> Option<&Value> {
        self.single.get(name)
    }

    /// Record one single-value edge. Kernel-internal.
    pub(crate) fn push_single(&mut self, name: String, value: Value) {
        self.single.insert(name, value);
    }

    /// Record one many-value edge. Kernel-internal.
    pub(crate) fn push_many(&mut self, requirement: &str, name: String, value: Value) {
        self.many
            .entry(requirement.to_owned())
            .or_default()
            .push((name, value));
    }

    /// Every resolved edge name, sorted.
    #[must_use]
    pub fn names(&self) -> Vec<String> {
        let mut names: Vec<String> = self.single.keys().cloned().collect();
        names.sort();
        names
    }
}

fn type_mismatch(name: &str, want: &str) -> Error {
    Error::validation(format!("dependency {name:?} does not provide {want}"))
}

/// What a factory receives.
pub struct BuildCtx<'a> {
    /// The resource's name in the document.
    pub name: &'a str,
    /// The settings subtree, already expanded; `None` when absent.
    pub settings: Option<&'a serde_json::Value>,
    /// The resolved dependencies declared by the factory's [`Spec`].
    pub deps: &'a ResolvedDeps,
    /// The document loader's base directory, for factories that resolve
    /// relative paths.
    pub base_dir: Option<&'a std::path::Path>,
    /// The deployment's secret table, so a [`secrets::Secret`] in the settings
    /// can resolve lazily. `None` means the graph installed no secret store.
    pub secrets: Option<&'a secrets::SecretTable>,
}

impl BuildCtx<'_> {
    /// Decode the settings subtree strictly, naming the failing field path.
    pub fn settings<T: serde::de::DeserializeOwned>(&self) -> Result<T> {
        match self.settings {
            Some(value) => crate::resource::settings(value),
            None => crate::resource::settings(&serde_json::Value::Object(Default::default())),
        }
    }
}
