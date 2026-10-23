//! Grant scope: an execution-id prefix signed into the token.

use flowcraft_error::{Error, Result};
use serde_json::Value;

/// What a grant allows on a checkpoint store.
#[derive(Debug, Clone, Default, serde::Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Scope {
    /// Execution ids must start with this prefix. Empty allows every id.
    #[serde(default)]
    pub prefix: String,
}

impl Scope {
    /// Decode a scope. A missing scope is the default scope.
    pub fn parse(value: &Value) -> Result<Self> {
        if value.is_null() {
            return Ok(Self::default());
        }
        flowcraft_resource::settings(value)
    }

    /// Whether this grant may touch `exec_id`.
    pub fn allows(&self, exec_id: &str) -> Result<()> {
        if self.prefix.is_empty() || exec_id.starts_with(&self.prefix) {
            return Ok(());
        }
        Err(Error::forbidden(format!(
            "exec_id {exec_id:?} is outside the grant scope {:?}",
            self.prefix
        )))
    }

    /// Keep only the ids this grant may see.
    #[must_use]
    pub fn filter(&self, ids: Vec<String>) -> Vec<String> {
        if self.prefix.is_empty() {
            return ids;
        }
        ids.into_iter()
            .filter(|id| id.starts_with(&self.prefix))
            .collect()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_prefix_confines_ids_and_filters_listing() {
        let scope = Scope::parse(&serde_json::json!({"prefix": "team-a/"})).unwrap();
        scope.allows("team-a/run-1").unwrap();
        assert_eq!(
            scope.allows("team-b/run-1").unwrap_err().kind(),
            flowcraft_error::ErrorKind::Forbidden
        );
        assert_eq!(
            scope.filter(vec!["team-a/run-1".to_owned(), "team-b/run-1".to_owned()]),
            vec!["team-a/run-1"]
        );
    }

    #[test]
    fn an_empty_prefix_allows_everything() {
        let scope = Scope::parse(&serde_json::json!({})).unwrap();
        scope.allows("anything").unwrap();
        assert_eq!(scope.filter(vec!["a".to_owned()]).len(), 1);
    }
}
