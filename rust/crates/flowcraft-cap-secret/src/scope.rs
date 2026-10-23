//! Grant scope: the name prefixes signed into a token.
//!
//! Two spellings are accepted, and both are additive:
//!
//! * `{"prefix": "team/"}` — this crate's original form.
//! * `{"prefixes": ["team/", "shared/"]}` — the Go side's form, which also
//!   allows several prefixes.
//!
//! A grant that carries neither allows every name. Accepting both matters for
//! interop: a scope silently ignored by the other end would widen the grant
//! instead of narrowing it.

use flowcraft_error::{Error, Result};
use serde::{Deserialize, Serialize};
use serde_json::Value;

/// What a grant allows on a secret store.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Scope {
    /// Secret names must start with this prefix. Empty allows every name.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub prefix: String,
    /// Secret names must start with one of these prefixes. Empty adds nothing.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub prefixes: Vec<String>,
}

impl Scope {
    /// Decode a scope. A missing scope is the default scope.
    pub fn parse(value: &Value) -> Result<Self> {
        if value.is_null() {
            return Ok(Self::default());
        }
        let scope: Self = flowcraft_resource::settings(value)?;
        Ok(scope)
    }

    /// Every prefix the grant declares, in a stable order, without empties.
    #[must_use]
    pub fn allowed_prefixes(&self) -> Vec<&str> {
        let mut out: Vec<&str> = Vec::new();
        if !self.prefix.is_empty() {
            out.push(&self.prefix);
        }
        for prefix in &self.prefixes {
            if !prefix.is_empty() && !out.contains(&prefix.as_str()) {
                out.push(prefix);
            }
        }
        out
    }

    /// Whether this grant may read `name`.
    pub fn allows(&self, name: &str) -> Result<()> {
        let allowed = self.allowed_prefixes();
        if allowed.is_empty() || allowed.iter().any(|prefix| name.starts_with(prefix)) {
            return Ok(());
        }
        Err(Error::forbidden(format!(
            "secret {name:?} is outside the grant scope {allowed:?}"
        )))
    }

    /// The wire form to sign into a grant.
    ///
    /// Both keys are emitted when both are meaningful, so an endpoint running
    /// the other implementation reads the same restriction it would read from
    /// one of its own tokens.
    #[must_use]
    pub fn wire(&self) -> Value {
        let allowed = self.allowed_prefixes();
        let mut out = serde_json::Map::new();
        out.insert(
            "prefixes".to_owned(),
            Value::Array(
                allowed
                    .iter()
                    .map(|prefix| Value::String((*prefix).to_owned()))
                    .collect(),
            ),
        );
        if !self.prefix.is_empty() {
            out.insert("prefix".to_owned(), Value::String(self.prefix.clone()));
        }
        Value::Object(out)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn an_empty_prefix_allows_everything() {
        let scope = Scope::parse(&serde_json::json!({})).unwrap();
        scope.allows("anything").unwrap();
        assert!(scope.allowed_prefixes().is_empty());
    }

    #[test]
    fn a_prefix_confines_names() {
        let scope = Scope::parse(&serde_json::json!({"prefix": "team/"})).unwrap();
        scope.allows("team/token").unwrap();
        assert_eq!(
            scope.allows("other/token").unwrap_err().kind(),
            flowcraft_error::ErrorKind::Forbidden
        );
    }

    #[test]
    fn several_prefixes_are_any_of() {
        let scope = Scope::parse(&serde_json::json!({"prefixes": ["team/", "shared/"]})).unwrap();
        scope.allows("team/token").unwrap();
        scope.allows("shared/token").unwrap();
        assert!(scope.allows("other/token").is_err());
        // An empty entry adds nothing rather than allowing everything.
        let scope = Scope::parse(&serde_json::json!({"prefixes": [""]})).unwrap();
        scope.allows("anything").unwrap();
    }

    #[test]
    fn both_spellings_combine() {
        let scope = Scope::parse(&serde_json::json!({"prefix": "a/", "prefixes": ["b/"]})).unwrap();
        scope.allows("a/x").unwrap();
        scope.allows("b/x").unwrap();
        assert!(scope.allows("c/x").is_err());
        assert_eq!(scope.allowed_prefixes(), vec!["a/", "b/"]);
    }

    #[test]
    fn the_wire_form_names_the_prefix_list() {
        let scope = Scope::parse(&serde_json::json!({"prefix": "team/"})).unwrap();
        let wire = scope.wire();
        assert_eq!(wire["prefixes"], serde_json::json!(["team/"]));
        assert_eq!(wire["prefix"], serde_json::json!("team/"));
        // What it emits parses back to the same restriction (the two spellings
        // are additive, so the round trip carries the prefix twice).
        let back = Scope::parse(&wire).unwrap();
        assert_eq!(back.allowed_prefixes(), scope.allowed_prefixes());
    }

    #[test]
    fn unknown_fields_are_rejected() {
        assert!(Scope::parse(&serde_json::json!({"prefix": "x", "all": true})).is_err());
    }
}
