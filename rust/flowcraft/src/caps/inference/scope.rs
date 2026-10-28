//! What a grant allows this caller to do with the inference capability.
//!
//! A scope only ever *narrows*: a caller may name a model the deployment declared, or a
//! tier the deployment defined — never one it did not, and never a ceiling above the
//! deployment's. The two halves are separate because they answer different questions:
//! a model is an identity, a tier is a routing class the deployment defined, and a caller
//! that may use one is not automatically allowed the other.
//!
//! ```json
//! {"models": ["gpt-5"], "tiers": ["standard"], "max_output_tokens": 2048}
//! ```
//!
//! An absent list means *none*, not "all": a scope is a permission, and an empty
//! permission grants nothing.

use serde::Deserialize;

use crate::error::{Error, Result};

/// The parsed grant scope.
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Scope {
    /// Exact model names this caller may use.
    #[serde(default)]
    pub models: Vec<String>,
    /// Routing classes this caller may name.
    #[serde(default)]
    pub tiers: Vec<String>,
    /// A ceiling on `max_output_tokens`. The tighter of this and the request's wins.
    #[serde(default)]
    pub max_output_tokens: Option<u64>,
}

impl Scope {
    /// Parse a grant's scope value.
    pub fn parse(value: &serde_json::Value) -> Result<Self> {
        let scope: Self = serde_json::from_value(value.clone())
            .map_err(|error| Error::validation(format!("inference: grant scope: {error}")))?;
        for (kind, names) in [("models", &scope.models), ("tiers", &scope.tiers)] {
            for name in names {
                if name.trim().is_empty() || name.trim() != name {
                    return Err(Error::validation(format!(
                        "inference: grant scope {kind} contains {name:?}"
                    )));
                }
            }
        }
        if scope.max_output_tokens == Some(0) {
            return Err(Error::validation(
                "inference: grant scope max_output_tokens must be positive",
            ));
        }
        Ok(scope)
    }

    /// Whether the caller may name this model.
    #[must_use]
    pub fn allows_model(&self, model: &str) -> bool {
        self.models.iter().any(|allowed| allowed == model)
    }

    /// Whether the caller may name this tier.
    #[must_use]
    pub fn allows_tier(&self, tier: &str) -> bool {
        self.tiers.iter().any(|allowed| allowed == tier)
    }

    /// The ceiling this scope imposes on a request that asks for less, or for nothing.
    #[must_use]
    pub fn output_limit(&self, requested: Option<u64>) -> Option<u64> {
        match (self.max_output_tokens, requested) {
            (None, requested) => requested,
            (Some(limit), None) => Some(limit),
            (Some(limit), Some(requested)) => Some(limit.min(requested)),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn scope(json: serde_json::Value) -> Scope {
        Scope::parse(&json).expect("scope")
    }

    #[test]
    fn a_scope_names_what_it_allows_and_nothing_else() {
        let scope = scope(serde_json::json!({
            "models": ["gpt-5"],
            "tiers": ["standard"],
        }));
        assert!(scope.allows_model("gpt-5"));
        assert!(!scope.allows_model("gpt-6"));
        assert!(scope.allows_tier("standard"));
        assert!(!scope.allows_tier("premium"));
    }

    /// An absent list is a permission that grants nothing — the fail-closed reading, and
    /// the opposite of "absent means all".
    #[test]
    fn an_empty_scope_allows_nothing() {
        let scope = Scope::default();
        assert!(!scope.allows_model("gpt-5"));
        assert!(!scope.allows_tier("standard"));
        assert_eq!(scope.output_limit(Some(64)), Some(64));
    }

    #[test]
    fn a_ceiling_only_ever_tightens() {
        let scope = scope(serde_json::json!({"models": ["gpt-5"], "max_output_tokens": 2048}));
        assert_eq!(
            scope.output_limit(None),
            Some(2048),
            "the scope fills a gap"
        );
        assert_eq!(scope.output_limit(Some(4096)), Some(2048), "the scope caps");
        assert_eq!(scope.output_limit(Some(64)), Some(64), "never raises");
    }

    #[test]
    fn a_scope_with_a_bad_name_or_a_zero_ceiling_is_refused() {
        for json in [
            serde_json::json!({"models": [""]}),
            serde_json::json!({"tiers": [" standard"]}),
            serde_json::json!({"max_output_tokens": 0}),
            serde_json::json!({"model": ["gpt-5"]}),
        ] {
            assert!(Scope::parse(&json).is_err(), "{json}");
        }
    }
}
