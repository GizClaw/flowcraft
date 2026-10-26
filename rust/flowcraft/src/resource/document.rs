//! The deployment document.
//!
//! One document, two namespaces: `resources` (things the graph constructs) and
//! `externals` (values the host injects). `entries` names what the host wants
//! back. `runtime` is opaque to this crate — the platform kernel decodes it.
//!
//! There is deliberately no `agents:` section: in the two-plane architecture an
//! agent is a craft-SDK concept, and the platform document never describes one.

use serde::{Deserialize, Deserializer, Serialize};
use serde_json::Value;
use std::collections::BTreeMap;

use crate::error::{Error, Result};

/// Rejects names that would be ambiguous once they travel through a document
/// path, a dependency key, or a gRPC field.
pub fn validate_name(what: &str, name: &str) -> Result<()> {
    if name.trim().is_empty() {
        return Err(Error::validation(format!("{what}: name is empty")));
    }
    if name != name.trim() {
        return Err(Error::validation(format!(
            "{what} {name:?}: name has surrounding whitespace"
        )));
    }
    if name.contains(['/', '\\', ' ', '\t', '\r', '\n']) {
        return Err(Error::validation(format!(
            "{what} {name:?}: name must not contain '/' or whitespace"
        )));
    }
    Ok(())
}

/// A resource type, e.g. `workspace.Workspace` or `event.Bus`.
///
/// A dotted name, not a package path: it is the identity the Go side and the
/// document share, so it is validated on construction and on deserialization.
#[derive(Debug, Clone, PartialEq, Eq, PartialOrd, Ord, Hash, Serialize)]
#[serde(transparent)]
pub struct Kind(String);

impl Kind {
    /// Validate and wrap a kind.
    pub fn new(value: impl Into<String>) -> Result<Self> {
        let value = value.into();
        validate_name("kind", &value)?;
        Ok(Self(value))
    }

    /// The raw kind.
    #[must_use]
    pub fn as_str(&self) -> &str {
        &self.0
    }
}

impl std::fmt::Display for Kind {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

impl<'de> Deserialize<'de> for Kind {
    fn deserialize<D: Deserializer<'de>>(deserializer: D) -> std::result::Result<Self, D::Error> {
        let raw = String::deserialize(deserializer)?;
        Self::new(raw).map_err(serde::de::Error::custom)
    }
}

/// A producer reference: the name of another resource or of an external.
#[derive(Debug, Clone, PartialEq, Eq, PartialOrd, Ord, Hash, Serialize)]
#[serde(transparent)]
pub struct Ref(String);

impl Ref {
    /// Validate and wrap a reference.
    pub fn new(value: impl Into<String>) -> Result<Self> {
        let value = value.into();
        validate_name("dependency", &value)?;
        Ok(Self(value))
    }

    /// The raw reference.
    #[must_use]
    pub fn as_str(&self) -> &str {
        &self.0
    }
}

impl std::fmt::Display for Ref {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

impl<'de> Deserialize<'de> for Ref {
    fn deserialize<D: Deserializer<'de>>(deserializer: D) -> std::result::Result<Self, D::Error> {
        let raw = String::deserialize(deserializer)?;
        Self::new(raw).map_err(serde::de::Error::custom)
    }
}

/// A consumer's dependency name mapped to the producer it refers to.
pub type Deps = BTreeMap<String, Ref>;

/// One resource in the document.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Resource {
    /// What it is.
    pub kind: Kind,
    /// Which implementation of that kind, when the kind has more than one.
    #[serde(default, rename = "impl", skip_serializing_if = "Option::is_none")]
    pub imp: Option<String>,
    /// Dependency edges, keyed by the consumer's dependency name.
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub deps: Deps,
    /// The factory's own settings subtree, opaque to this crate.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub settings: Option<Value>,
}

impl Resource {
    /// Checks the static shape. Type checking against a registry is `Plan`'s job.
    pub fn validate(&self) -> Result<()> {
        if let Some(imp) = &self.imp {
            if imp.trim().is_empty() {
                return Err(Error::validation(format!("{}: impl is empty", self.kind)));
            }
            if imp != imp.trim() || imp.contains(['/', '\\', ' ', '\t', '\r', '\n']) {
                return Err(Error::validation(format!(
                    "{}: impl {imp:?} must not contain '/' or whitespace",
                    self.kind
                )));
            }
        }
        for name in self.deps.keys() {
            validate_name("dependency", name)?;
        }
        // Settings must be an object (or absent): a scalar would silently never
        // match any factory's expectations and only fail later, at Build.
        if let Some(settings) = &self.settings
            && !settings.is_object()
        {
            return Err(Error::validation(format!(
                "{}: settings must be an object",
                self.kind
            )));
        }
        Ok(())
    }

    /// The `kind/impl` identity used for registry lookup and diagnostics.
    #[must_use]
    pub fn key(&self) -> String {
        match &self.imp {
            Some(imp) if !imp.is_empty() => format!("{}/{}", self.kind, imp),
            _ => self.kind.to_string(),
        }
    }
}

/// One external the document declares. The host must inject a matching value.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct External {
    /// The dependency name a resource uses to refer to it.
    pub name: String,
    /// The contract the injected value must satisfy.
    pub contract: String,
}

impl External {
    /// Checks the declaration shape.
    pub fn validate(&self) -> Result<()> {
        validate_name("external", &self.name)?;
        if self.contract.trim().is_empty() {
            return Err(Error::validation(format!(
                "external {:?}: contract is empty",
                self.name
            )));
        }
        if self.contract != self.contract.trim() || self.contract.contains([' ', '\t', '\r', '\n'])
        {
            return Err(Error::validation(format!(
                "external {:?}: contract {:?} must not contain whitespace",
                self.name, self.contract
            )));
        }
        Ok(())
    }
}

/// The whole document.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Deployment {
    /// Document schema version. Layers and future migrations key off it.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub version: Option<String>,
    /// The graph.
    #[serde(default)]
    pub resources: BTreeMap<String, Resource>,
    /// What the host asks back after a successful build.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub entries: Vec<String>,
    /// Host-injected values the graph may depend on.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub externals: Vec<External>,
    /// Kernel configuration; opaque here.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub runtime: Option<Value>,
}

impl Deployment {
    /// Checks names, resource shapes, entries, and externals.
    ///
    /// Cross-resource checks (unknown dependency, missing factory, type
    /// mismatch, cycle) belong to `Plan`, which owns the registry.
    pub fn validate(&self) -> Result<()> {
        for (name, resource) in &self.resources {
            validate_name("resource", name)?;
            resource
                .validate()
                .map_err(|error| Error::new(error.kind(), format!("resources.{name}: {error}")))?;
        }

        let mut seen_entries = std::collections::BTreeSet::new();
        for (index, entry) in self.entries.iter().enumerate() {
            validate_name("entry", entry)
                .map_err(|error| Error::new(error.kind(), format!("entries[{index}]: {error}")))?;
            if !seen_entries.insert(entry) {
                return Err(Error::conflict(format!(
                    "entries: duplicate entry {entry:?}"
                )));
            }
            if !self.resources.contains_key(entry) {
                return Err(Error::not_found(format!(
                    "entries: {entry:?} names no resource"
                )));
            }
        }

        let mut seen_externals = std::collections::BTreeSet::new();
        for (index, external) in self.externals.iter().enumerate() {
            external.validate().map_err(|error| {
                Error::new(error.kind(), format!("externals[{index}]: {error}"))
            })?;
            if !seen_externals.insert(&external.name) {
                return Err(Error::conflict(format!(
                    "externals: duplicate external {:?}",
                    external.name
                )));
            }
            if self.resources.contains_key(&external.name) {
                return Err(Error::conflict(format!(
                    "externals: {:?} collides with a resource name",
                    external.name
                )));
            }
        }
        if let Some(runtime) = &self.runtime
            && !runtime.is_null()
            && !runtime.is_object()
        {
            return Err(Error::validation("runtime must be an object"));
        }
        Ok(())
    }

    /// Whether `name` is provided by this document (as a resource or external).
    #[must_use]
    pub fn provides(&self, name: &str) -> bool {
        self.resources.contains_key(name)
            || self.externals.iter().any(|external| external.name == name)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn resource(kind: &str, impl_: Option<&str>) -> Resource {
        Resource {
            kind: Kind::new(kind).unwrap(),
            imp: impl_.map(str::to_owned),
            deps: Deps::new(),
            settings: None,
        }
    }

    #[test]
    fn kind_rejects_slashes_and_whitespace() {
        assert!(Kind::new("workspace.Workspace").is_ok());
        assert!(Kind::new("").is_err());
        assert!(Kind::new("workspace/Workspace").is_err());
        assert!(Kind::new(" workspace.Workspace ").is_err());
    }

    #[test]
    fn duplicate_entry_is_a_conflict() {
        let document = Deployment {
            resources: BTreeMap::from([("ws".to_owned(), resource("workspace.Workspace", None))]),
            entries: vec!["ws".to_owned(), "ws".to_owned()],
            ..Deployment::default()
        };
        let error = document.validate().unwrap_err();
        assert_eq!(error.kind(), crate::error::ErrorKind::Conflict);
    }

    #[test]
    fn entry_must_name_a_resource() {
        let document = Deployment {
            entries: vec!["missing".to_owned()],
            ..Deployment::default()
        };
        assert_eq!(
            document.validate().unwrap_err().kind(),
            crate::error::ErrorKind::NotFound
        );
    }

    #[test]
    fn external_cannot_shadow_a_resource() {
        let document = Deployment {
            resources: BTreeMap::from([("ws".to_owned(), resource("workspace.Workspace", None))]),
            externals: vec![External {
                name: "ws".to_owned(),
                contract: "workspace.Workspace/v1".to_owned(),
            }],
            ..Deployment::default()
        };
        assert_eq!(
            document.validate().unwrap_err().kind(),
            crate::error::ErrorKind::Conflict
        );
    }

    #[test]
    fn settings_must_be_an_object() {
        let mut item = resource("workspace.Workspace", Some("local"));
        item.settings = Some(Value::String("nope".to_owned()));
        let error = item.validate().unwrap_err();
        assert_eq!(error.kind(), crate::error::ErrorKind::Validation);
    }

    #[test]
    fn runtime_must_be_an_object() {
        let document = Deployment {
            runtime: Some(Value::String("nope".to_owned())),
            ..Deployment::default()
        };
        assert_eq!(
            document.validate().unwrap_err().kind(),
            crate::error::ErrorKind::Validation
        );
        let document = Deployment {
            runtime: Some(serde_json::json!({"sessions": {}})),
            ..Deployment::default()
        };
        document.validate().unwrap();
    }

    #[test]
    fn key_names_the_impl_when_present() {
        assert_eq!(
            resource("workspace.Workspace", Some("local")).key(),
            "workspace.Workspace/local"
        );
        assert_eq!(
            resource("workspace.Workspace", None).key(),
            "workspace.Workspace"
        );
    }
}
