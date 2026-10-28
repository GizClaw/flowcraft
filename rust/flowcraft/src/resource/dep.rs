//! Dependency requirements.

use std::any::{Any, TypeId};

/// One dependency a factory declares.
///
/// `ty: None` is the explicit escape hatch for test doubles and hosts that
/// forward a value they do not model; the Go side spells this
/// `Requirement.Type == any`. Everything else is checked by [`crate::resource::Plan`].
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Requirement {
    /// The dependency name inside the factory's `Input`.
    pub name: &'static str,
    /// The Go type the producer must provide, when checked.
    pub ty: Option<TypeId>,
    /// A readable spelling of `ty`, for diagnostics.
    pub ty_name: &'static str,
    /// The cross-process contract, when the edge travels a wire.
    pub contract: Option<&'static str>,
    /// Whether the dependency must be supplied.
    pub required: bool,
    /// Whether edges named `name` or `name.<suffix>` all contribute.
    pub many: bool,
}

impl Requirement {
    /// Whether this requirement is satisfied by an edge with this name.
    #[must_use]
    pub fn claims(&self, edge: &str) -> bool {
        if edge == self.name {
            return true;
        }
        self.many
            && edge
                .strip_prefix(self.name)
                .is_some_and(|rest| rest.starts_with('.'))
    }
}

/// Declares a mandatory dependency.
#[must_use]
pub fn require<T: Any + Send + Sync>(name: &'static str) -> Requirement {
    typed::<T>(name, true, false)
}

/// Declares an optional dependency.
#[must_use]
pub fn optional<T: Any + Send + Sync>(name: &'static str) -> Requirement {
    typed::<T>(name, false, false)
}

/// Declares a mandatory collection dependency: edges named `name` or
/// `name.<suffix>`.
#[must_use]
pub fn require_many<T: Any + Send + Sync>(name: &'static str) -> Requirement {
    typed::<T>(name, true, true)
}

/// Declares an optional collection dependency.
#[must_use]
pub fn optional_many<T: Any + Send + Sync>(name: &'static str) -> Requirement {
    typed::<T>(name, false, true)
}

/// Declares a dependency whose type is not checked.
#[must_use]
pub fn require_any(name: &'static str) -> Requirement {
    Requirement {
        name,
        ty: None,
        ty_name: "any",
        contract: None,
        required: true,
        many: false,
    }
}

fn typed<T: Any + Send + Sync>(name: &'static str, required: bool, many: bool) -> Requirement {
    Requirement {
        name,
        ty: Some(TypeId::of::<T>()),
        ty_name: std::any::type_name::<T>(),
        contract: None,
        required,
        many,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn many_claims_prefixed_edges_only() {
        let requirement = require_many::<u8>("tool");
        assert!(requirement.claims("tool"));
        assert!(requirement.claims("tool.alpha"));
        assert!(!requirement.claims("tools"));
        assert!(!requirement.claims("toolbox.x"));
    }

    #[test]
    fn single_claims_exact_name_only() {
        let requirement = require::<u8>("tool");
        assert!(requirement.claims("tool"));
        assert!(!requirement.claims("tool.alpha"));
    }
}
