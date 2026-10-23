//! Grant scope: which tools a lease may call, and how many times.

use flowcraft_error::{Error, Result};
use flowcraft_proto::message as msg;
use serde_json::Value;

/// What a grant allows on a tool source.
#[derive(Debug, Clone, Default, serde::Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Scope {
    /// Tool names this grant may call. Empty allows every registered tool.
    #[serde(default)]
    pub allow: Vec<String>,
    /// Lease call budget, shared across renewals of the same session. `None` is
    /// unlimited; `Some(0)` denies every call, which is how a lease is handed
    /// out disabled.
    #[serde(default)]
    pub max_calls: Option<u64>,
}

impl Scope {
    /// Decode a scope. A missing scope is the default scope.
    pub fn parse(value: &Value) -> Result<Self> {
        if value.is_null() {
            return Ok(Self::default());
        }
        let mut scope: Self = flowcraft_resource::settings(value)?;
        scope.allow.sort_unstable();
        scope.allow.dedup();
        if let Some(name) = scope.allow.iter().find(|name| name.trim().is_empty()) {
            return Err(Error::validation(format!(
                "grant scope lists an empty tool name ({name:?})"
            )));
        }
        Ok(scope)
    }

    /// Whether this grant may call `name`.
    pub fn allows(&self, name: &str) -> Result<()> {
        if self.allow.is_empty() || self.allow.iter().any(|allowed| allowed == name) {
            return Ok(());
        }
        Err(Error::forbidden(format!(
            "tool {name:?} is outside the grant scope {:?}",
            self.allow
        )))
    }

    /// Keep only the definitions this grant may see.
    #[must_use]
    pub fn filter(&self, definitions: Vec<msg::ToolDefinition>) -> Vec<msg::ToolDefinition> {
        if self.allow.is_empty() {
            return definitions;
        }
        definitions
            .into_iter()
            .filter(|definition| self.allow.iter().any(|name| name == &definition.name))
            .collect()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn definition(name: &str) -> msg::ToolDefinition {
        msg::ToolDefinition {
            name: name.to_owned(),
            description: String::new(),
            input_schema: Vec::new(),
        }
    }

    #[test]
    fn an_allow_list_confines_calls_and_listing() {
        let scope =
            Scope::parse(&serde_json::json!({"allow": ["search"], "max_calls": 5})).unwrap();
        scope.allows("search").unwrap();
        assert_eq!(
            scope.allows("shell").unwrap_err().kind(),
            flowcraft_error::ErrorKind::Forbidden
        );
        let visible = scope.filter(vec![definition("search"), definition("shell")]);
        assert_eq!(
            visible
                .iter()
                .map(|item| item.name.as_str())
                .collect::<Vec<_>>(),
            vec!["search"]
        );
        assert_eq!(scope.max_calls, Some(5));
    }

    #[test]
    fn an_empty_allow_list_allows_everything() {
        let scope = Scope::parse(&serde_json::json!({})).unwrap();
        scope.allows("anything").unwrap();
        assert_eq!(scope.filter(vec![definition("a")]).len(), 1);
        assert_eq!(scope.max_calls, None);
    }

    #[test]
    fn an_unknown_field_is_refused() {
        assert!(Scope::parse(&serde_json::json!({"tool": ["x"]})).is_err());
    }
}
