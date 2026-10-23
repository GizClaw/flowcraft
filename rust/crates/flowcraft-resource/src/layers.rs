//! Layered deployment documents.
//!
//! A deployment is often assembled from a base document and one overlay per
//! environment. Layers are merged in order: a later layer replaces a resource
//! or external by name, appends new entries, and may bump the version. Each
//! layer is decoded strictly but validated only after merging, so a partial
//! overlay is legal.

use std::collections::BTreeMap;

use crate::{Deployment, decode};
use flowcraft_error::{Error, Result};
use serde_json::Value;

/// One document in a layered load.
#[derive(Debug, Clone)]
pub struct Layer {
    /// Where it came from, for diagnostics and provenance.
    pub name: String,
    /// The document bytes (JSON or YAML).
    pub data: Vec<u8>,
}

impl Layer {
    /// Build a layer from raw bytes.
    #[must_use]
    pub fn new(name: impl Into<String>, data: impl Into<Vec<u8>>) -> Self {
        Self {
            name: name.into(),
            data: data.into(),
        }
    }
}

/// Which layer last defined each item.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Provenance {
    /// Resource name → layer name.
    pub resources: BTreeMap<String, String>,
    /// External name → layer name.
    pub externals: BTreeMap<String, String>,
    /// Entry name → layer name.
    pub entries: BTreeMap<String, String>,
}

/// Merge layers in order and validate the result.
pub fn load(layers: &[Layer]) -> Result<(Deployment, Provenance)> {
    if layers.is_empty() {
        return Err(Error::validation("deployment needs at least one layer"));
    }
    let mut merged = Deployment::default();
    let mut provenance = Provenance::default();

    for layer in layers {
        let text = std::str::from_utf8(&layer.data).map_err(|error| {
            Error::validation(format!("layer {:?} is not UTF-8: {error}", layer.name))
        })?;
        let value = decode::document_value(text).map_err(|error| {
            Error::new(error.kind(), format!("layer {:?}: {error}", layer.name))
        })?;
        let deployment: Deployment = crate::settings(&value).map_err(|error| {
            Error::new(error.kind(), format!("layer {:?}: {error}", layer.name))
        })?;

        if let Some(version) = deployment.version {
            merged.version = Some(version);
        }
        for (name, resource) in deployment.resources {
            provenance
                .resources
                .insert(name.clone(), layer.name.clone());
            merged.resources.insert(name, resource);
        }
        for external in deployment.externals {
            provenance
                .externals
                .insert(external.name.clone(), layer.name.clone());
            match merged
                .externals
                .iter_mut()
                .find(|existing| existing.name == external.name)
            {
                Some(existing) => *existing = external,
                None => merged.externals.push(external),
            }
        }
        for entry in deployment.entries {
            if !merged.entries.contains(&entry) {
                provenance.entries.insert(entry.clone(), layer.name.clone());
                merged.entries.push(entry);
            }
        }
        if let Some(runtime) = deployment.runtime {
            merged.runtime = Some(runtime);
        }
    }

    merged.validate()?;
    Ok((merged, provenance))
}

/// Parse a document from bytes without merging (single-layer convenience).
pub fn single(name: impl Into<String>, data: Vec<u8>) -> Result<(Deployment, Provenance)> {
    load(&[Layer::new(name, data)])
}

/// Render provenance for a log line.
#[must_use]
pub fn summarize(provenance: &Provenance) -> String {
    let mut parts = Vec::new();
    for (name, layer) in &provenance.resources {
        parts.push(format!("{name}<{layer}"));
    }
    parts.join(", ")
}

/// A JSON view of provenance, for admin output.
#[must_use]
pub fn to_json(provenance: &Provenance) -> Value {
    serde_json::json!({
        "resources": provenance.resources,
        "externals": provenance.externals,
        "entries": provenance.entries,
    })
}
