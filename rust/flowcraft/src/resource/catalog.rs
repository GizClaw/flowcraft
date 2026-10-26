//! A serializable snapshot of what a registry can build.
//!
//! This is the machine-readable capability list. The Go side keeps it in a
//! hand-written table, which drifts; here the list *is* the registry.

use serde::Serialize;

use crate::resource::registry::Registry;

/// The capability list.
#[derive(Debug, Clone, Serialize)]
pub struct Catalog {
    /// Entries in `(kind, impl)` order.
    pub entries: Vec<CatalogEntry>,
}

/// One registered factory.
#[derive(Debug, Clone, Serialize)]
pub struct CatalogEntry {
    /// The resource type.
    pub kind: String,
    /// The implementation name, absent when unique.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub imp: Option<String>,
    /// The cross-process contract, when the value travels a wire.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub contract: Option<String>,
    /// `pre` or `main`.
    pub phase: String,
    /// The concrete Go/Rust build type, for diagnostics.
    pub build_type: String,
    /// Declared dependencies.
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub deps: Vec<CatalogDep>,
}

/// One declared dependency.
#[derive(Debug, Clone, Serialize)]
pub struct CatalogDep {
    /// The dependency name.
    pub name: String,
    /// The required type, when checked.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub ty: Option<String>,
    /// The required contract, when pinned.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub contract: Option<String>,
    /// Whether it must be supplied.
    pub required: bool,
    /// Whether `name.*` edges also contribute.
    pub many: bool,
}

impl Catalog {
    /// Snapshot a registry.
    #[must_use]
    pub fn from_registry(registry: &Registry) -> Self {
        let mut entries = Vec::new();
        for spec in registry.specs() {
            let build_type = registry.get(&spec.kind, spec.imp).map_or_else(
                || "unknown".to_owned(),
                |factory| factory.build_type_name().to_owned(),
            );
            entries.push(CatalogEntry {
                kind: spec.kind.to_string(),
                imp: spec.imp.map(str::to_owned),
                contract: spec.contract.map(str::to_owned),
                phase: spec.phase.as_str().to_owned(),
                build_type,
                deps: spec
                    .deps
                    .iter()
                    .map(|requirement| CatalogDep {
                        name: requirement.name.to_owned(),
                        ty: requirement.ty.map(|_| requirement.ty_name.to_owned()),
                        contract: requirement.contract.map(str::to_owned),
                        required: requirement.required,
                        many: requirement.many,
                    })
                    .collect(),
            });
        }
        Self { entries }
    }
}
