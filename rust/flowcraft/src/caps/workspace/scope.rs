//! Grant scope: the limits signed into the token.

use crate::error::Result;
use serde_json::Value;

use crate::caps::workspace::codes::{self, Code};

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
    ///
    /// The root is cleaned here, once per call path, so [`Scope::allows`] can
    /// compare two already-normalized strings. A root that cannot be cleaned
    /// (absolute, or escaping) fails the decode rather than being interpreted.
    pub fn parse(value: &Value) -> Result<Self> {
        if value.is_null() {
            return Ok(Self::default());
        }
        let mut scope: Self = crate::resource::settings(value)?;
        scope.root = crate::caps::workspace::path::clean(&scope.root)?;
        Ok(scope)
    }

    /// Whether `path` may be touched, and whether a write is allowed.
    ///
    /// The caller's path is cleaned first, exactly as the provider will clean
    /// it: comparing the raw string would let `repos/../etc/passwd` match a
    /// `repos` root and then be resolved *outside* it by the provider. A path
    /// that cannot be cleaned is refused — the grant cannot allow what the
    /// provider will not accept, and it is already [`ErrorKind::Forbidden`].
    ///
    /// [`ErrorKind::Forbidden`]: crate::error::ErrorKind::Forbidden
    pub fn allows(&self, path: &str, write: bool) -> Result<()> {
        if write && !self.write {
            return Err(codes::forbidden(
                Code::ScopeReadOnly,
                "grant scope is read-only",
            ));
        }
        let cleaned = crate::caps::workspace::path::clean(path)?;
        if self.root.is_empty() {
            return Ok(());
        }
        if cleaned == self.root || cleaned.starts_with(&format!("{}/", self.root)) {
            return Ok(());
        }
        Err(codes::forbidden(
            Code::ScopeDenied,
            format!("path {path:?} is outside the grant scope {:?}", self.root),
        ))
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
            crate::error::ErrorKind::Forbidden
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
            crate::error::ErrorKind::Forbidden
        );
    }

    /// Regression: the comparison used to be on the raw path, so a caller could
    /// match the root prefix and then have the provider resolve past it.
    #[test]
    fn an_interior_parent_dir_cannot_step_out_of_the_root() {
        let scope = Scope::parse(&serde_json::json!({"root": "repos", "write": true})).unwrap();
        assert_eq!(
            scope
                .allows("repos/../etc/passwd", false)
                .unwrap_err()
                .kind(),
            crate::error::ErrorKind::Forbidden
        );
        assert_eq!(
            scope
                .allows("repos/../../etc/passwd", true)
                .unwrap_err()
                .kind(),
            crate::error::ErrorKind::Forbidden
        );
        // A path that stays inside after cleaning is still allowed.
        scope.allows("repos/a/../b.txt", true).unwrap();
    }

    #[test]
    fn an_absolute_or_escaping_root_fails_the_decode() {
        for root in ["/etc", "../outside"] {
            let value = serde_json::json!({"root": root});
            assert_eq!(
                Scope::parse(&value).unwrap_err().kind(),
                crate::error::ErrorKind::Forbidden,
                "{root}"
            );
        }
    }

    #[test]
    fn unknown_scope_fields_are_rejected() {
        let error = Scope::parse(&serde_json::json!({"root": "x", "admin": true})).unwrap_err();
        assert_eq!(error.kind(), crate::error::ErrorKind::Validation);
    }
}
