//! Injection policy: which tools reach the model, and how big the discovery
//! pool may grow.
//!
//! Ported from `core/tool/exposure.go`. The policy is host-declared per
//! assembly; zero fields fall back to the defaults, so a partial settings block
//! is enough.

use std::collections::BTreeMap;

use crate::error::{Error, Result};
use serde::Deserialize;
use serde_json::Value;

/// Whether a tool appears in the model-visible definitions.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Exposure {
    /// Visible every turn. Keep this set small: it is the stable baseline.
    Always,
    /// Visible when required by name or sitting in the discovery pool.
    Direct,
    /// Hidden until `tool_search` surfaces it; kept while used, then expired.
    #[default]
    Deferred,
    /// Never visible, not even through search. Still callable by exact name.
    Hidden,
}

impl Exposure {
    /// Eviction rank: lower ranks are evicted last.
    #[must_use]
    pub fn rank(self) -> u8 {
        match self {
            Self::Always => 0,
            Self::Direct => 1,
            Self::Deferred => 2,
            Self::Hidden => 3,
        }
    }

    /// Whether `tool_search` may surface this exposure.
    #[must_use]
    pub fn searchable(self) -> bool {
        matches!(self, Self::Direct | Self::Deferred)
    }
}

/// Caps how many definitions reach the model per turn.
///
/// The zero value means "unset": [`Policy::normalized`] fills it with
/// [`Budget::recommended`].
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Budget {
    /// Maximum number of definitions. Zero falls back to the default.
    #[serde(default)]
    pub max_definitions: usize,
    /// Maximum total serialized definition size. Zero falls back to the default.
    #[serde(default)]
    pub max_bytes: i64,
}

impl Budget {
    /// The built-in budget: 32 tools or 16 KiB of definitions per turn.
    #[must_use]
    pub fn recommended() -> Self {
        Self {
            max_definitions: 32,
            max_bytes: 16 * 1024,
        }
    }
}

/// Bounds the session discovery pool.
///
/// The pool budget is independent of [`Budget`]: the budget caps what reaches
/// the model this turn, discovery caps what the session remembers.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DiscoveryPolicy {
    /// Maximum pool entries. Zero falls back to the effective budget.
    #[serde(default)]
    pub max_tools: usize,
    /// Maximum total serialized definition size. Zero falls back to the budget.
    #[serde(default)]
    pub max_bytes: i64,
    /// Evict an entry unused for this many rounds. Zero falls back to ten.
    #[serde(default)]
    pub idle_rounds: usize,
}

impl DiscoveryPolicy {
    /// The recommended pool bounds: the default budget, with a 10-round idle
    /// horizon.
    #[must_use]
    pub fn recommended() -> Self {
        Self {
            max_tools: Budget::recommended().max_definitions,
            max_bytes: Budget::recommended().max_bytes,
            idle_rounds: 10,
        }
    }
}

/// The host-declared injection policy for one assembly.
#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Policy {
    /// Exposure for tools without an explicit entry in [`Self::exposures`].
    #[serde(default)]
    pub default: Exposure,
    /// Per-tool exposure overrides.
    #[serde(default)]
    pub exposures: BTreeMap<String, Exposure>,
    /// Per-round visible budget.
    #[serde(default)]
    pub budget: Budget,
    /// Load every deferred tool before ranking in `tool_search`.
    #[serde(default, rename = "search_with_load")]
    pub search_with_load: bool,
    /// Discovery pool bounds.
    #[serde(default)]
    pub discovery: DiscoveryPolicy,
    /// Deprecated idle-horizon seed; used only when `discovery.idle_rounds` is
    /// unset.
    #[serde(default)]
    pub selected_retention: usize,
    /// Deprecated idle-horizon seed; used when the other two are unset.
    #[serde(default)]
    pub recent_window: usize,
}

impl Default for Policy {
    fn default() -> Self {
        Self {
            default: Exposure::Deferred,
            exposures: BTreeMap::new(),
            budget: Budget::default(),
            search_with_load: false,
            discovery: DiscoveryPolicy::default(),
            selected_retention: 0,
            recent_window: 0,
        }
    }
}

impl Policy {
    /// Decode a policy from settings. A missing policy is the default policy.
    pub fn parse(value: &Value) -> Result<Self> {
        if value.is_null() {
            return Ok(Self::default().normalized());
        }
        let policy: Self = crate::resource::settings(value)?;
        let policy = policy.normalized();
        policy.validate()?;
        Ok(policy)
    }

    /// Fill zero fields with defaults so settings can express a partial policy.
    #[must_use]
    pub fn normalized(mut self) -> Self {
        let budget = Budget::recommended();
        let discovery = DiscoveryPolicy::recommended();
        if self.budget.max_definitions == 0 {
            self.budget.max_definitions = budget.max_definitions;
        }
        if self.budget.max_bytes <= 0 {
            self.budget.max_bytes = budget.max_bytes;
        }
        // The pool must not be wider than one round's visible set; otherwise
        // pooled tools silently fall out of the model's definitions.
        if self.discovery.max_tools == 0 {
            self.discovery.max_tools = self.budget.max_definitions;
        }
        if self.discovery.max_bytes <= 0 {
            self.discovery.max_bytes = self.budget.max_bytes;
        }
        if self.discovery.idle_rounds == 0 {
            self.discovery.idle_rounds = match (self.selected_retention, self.recent_window) {
                (retention, _) if retention > 0 => retention,
                (_, window) if window > 0 => window,
                _ => discovery.idle_rounds,
            };
        }
        self
    }

    /// Check invariants. Call after [`Self::normalized`].
    pub fn validate(&self) -> Result<()> {
        for (name, exposure) in &self.exposures {
            if name.trim().is_empty() {
                return Err(Error::validation(
                    "tool policy: exposure map has an empty name",
                ));
            }
            if *name == crate::caps::tool::tool_search::TOOL_NAME && *exposure != Exposure::Always {
                return Err(Error::validation(format!(
                    "tool policy: {} must use exposure always; {exposure:?} disables discovery",
                    crate::caps::tool::tool_search::TOOL_NAME
                )));
            }
        }
        Ok(())
    }

    /// The exposure applied to one tool.
    #[must_use]
    pub fn exposure_of(&self, name: &str) -> Exposure {
        self.exposures.get(name).copied().unwrap_or(self.default)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_partial_policy_is_normalized_from_defaults() {
        let policy = Policy::parse(&serde_json::json!({"budget": {"max_definitions": 8}})).unwrap();
        assert_eq!(policy.default, Exposure::Deferred);
        assert_eq!(policy.budget.max_definitions, 8);
        assert_eq!(policy.budget.max_bytes, 16 * 1024);
        assert_eq!(policy.discovery.max_tools, 8);
        assert_eq!(policy.discovery.idle_rounds, 10);
    }

    #[test]
    fn a_missing_policy_is_the_default() {
        let policy = Policy::parse(&serde_json::Value::Null).unwrap();
        assert_eq!(policy.budget.max_definitions, 32);
        assert_eq!(policy.discovery.max_bytes, 16 * 1024);
    }

    #[test]
    fn tool_search_cannot_be_hidden() {
        let error = Policy::parse(&serde_json::json!({
            "exposures": { "tool_search": "hidden" }
        }))
        .unwrap_err();
        assert!(error.to_string().contains("tool_search"), "{error}");
    }

    #[test]
    fn an_unknown_field_is_refused() {
        assert!(Policy::parse(&serde_json::json!({"nope": 1})).is_err());
    }
}
