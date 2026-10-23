//! Grant scope: the limits signed into the token.

use flowcraft_error::{Error, Result};
use serde_json::Value;

/// What a grant allows on this capability.
#[derive(Debug, Clone, Default, serde::Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Scope {
    /// Paths must live under this prefix. Empty means the whole workspace.
    #[serde(default)]
    pub root: String,
    /// Whether write operations are allowed.
    #[serde(default)]
    pub write: bool,
}

impl Scope {
    /// Decode a scope. A missing scope is the default scope.
    pub fn parse(value: &Value) -> Result<Self> {
        if value.is_null() {
            return Ok(Self::default());
        }
        flowcraft_resource::settings(value)
    }

    /// Whether `path` may be touched, and whether a write is allowed.
    pub fn allows(&self, path: &str, write: bool) -> Result<()> {
        if write && !self.write {
            return Err(Error::forbidden("grant scope is read-only"));
        }
        if self.root.is_empty() {
            return Ok(());
        }
        let root = self.root.trim_matches('/');
        let path = path.trim_start_matches("./").trim_end_matches('/');
        if path == root || path.starts_with(&format!("{root}/")) {
            return Ok(());
        }
        Err(Error::forbidden(format!(
            "path {path:?} is outside the grant scope {root:?}"
        )))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn empty_scope_allows_reads_only() {
        let scope = Scope::parse(&serde_json::json!({})).unwrap();
        scope.allows("anything", false).unwrap();
        assert_eq!(
            scope.allows("anything", true).unwrap_err().kind(),
            flowcraft_error::ErrorKind::Forbidden
        );
    }

    #[test]
    fn root_confines_paths() {
        let scope = Scope::parse(&serde_json::json!({"root": "repos", "write": true})).unwrap();
        scope.allows("repos/a.txt", true).unwrap();
        scope.allows("repos", false).unwrap();
        assert_eq!(
            scope
                .allows("repositories/a.txt", false)
                .unwrap_err()
                .kind(),
            flowcraft_error::ErrorKind::Forbidden
        );
    }

    #[test]
    fn unknown_scope_fields_are_rejected() {
        let error = Scope::parse(&serde_json::json!({"root": "x", "admin": true})).unwrap_err();
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::Validation);
    }
}
