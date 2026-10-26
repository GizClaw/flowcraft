//! Grant scope: what a session may run.

use std::time::Duration;

use crate::error::{Error, Result};
use serde_json::Value;

use crate::caps::sandbox::codes::{self, Code};

/// Limits signed into a `sandbox.Runner/v1` grant.
#[derive(Debug, Clone, Default, serde::Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Scope {
    /// Allowed `argv[0]` values. Empty means "no command restriction".
    #[serde(default)]
    pub commands: Vec<String>,
    /// Upper bound on a session timeout.
    #[serde(default)]
    pub max_timeout_secs: Option<u64>,
    /// Upper bound on buffered output.
    #[serde(default)]
    pub max_output_bytes: Option<u64>,
}

impl Scope {
    /// Decode a scope. A missing scope is the default scope.
    pub fn parse(value: &Value) -> Result<Self> {
        if value.is_null() {
            return Ok(Self::default());
        }
        crate::resource::settings(value)
    }

    /// Whether this grant may run `argv`.
    pub fn allows_command(&self, argv: &[String]) -> Result<()> {
        if self.commands.is_empty() {
            return Ok(());
        }
        let Some(program) = argv.first() else {
            return Err(Error::validation("sandbox: argv is empty"));
        };
        if self.commands.iter().any(|allowed| allowed == program) {
            return Ok(());
        }
        Err(codes::forbidden(
            Code::CommandNotAllowed,
            format!("sandbox: {program:?} is outside the grant's command allow-list"),
        ))
    }

    /// Clamp a requested timeout to the grant's ceiling.
    pub fn timeout(&self, requested: Option<Duration>) -> Result<Option<Duration>> {
        let Some(max) = self.max_timeout_secs else {
            return Ok(requested);
        };
        let max = Duration::from_secs(max);
        match requested {
            Some(requested) if requested > max => Err(codes::forbidden(
                Code::TimeoutExceedsGrant,
                format!("sandbox: timeout {requested:?} exceeds the grant's {max:?}"),
            )),
            Some(requested) => Ok(Some(requested)),
            None => Ok(Some(max)),
        }
    }

    /// The output bound to apply, taking the stricter of grant and request.
    #[must_use]
    pub fn output_limit(&self, requested: Option<u64>) -> Option<u64> {
        match (self.max_output_bytes, requested) {
            (Some(grant), Some(requested)) => Some(grant.min(requested)),
            (Some(grant), None) => Some(grant),
            (None, requested) => requested,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn argv(program: &str) -> Vec<String> {
        vec![program.to_owned()]
    }

    #[test]
    fn commands_are_an_allow_list() {
        let scope = Scope::parse(&serde_json::json!({"commands": ["sh", "rg"]})).unwrap();
        scope.allows_command(&argv("sh")).unwrap();
        assert_eq!(
            scope.allows_command(&argv("rm")).unwrap_err().kind(),
            crate::error::ErrorKind::Forbidden
        );
    }

    #[test]
    fn an_empty_allow_list_permits_anything() {
        let scope = Scope::default();
        scope.allows_command(&argv("rm")).unwrap();
    }

    #[test]
    fn timeouts_are_capped_not_silently_shortened() {
        let scope = Scope::parse(&serde_json::json!({"max_timeout_secs": 60})).unwrap();
        assert_eq!(scope.timeout(None).unwrap(), Some(Duration::from_secs(60)));
        assert_eq!(
            scope.timeout(Some(Duration::from_secs(30))).unwrap(),
            Some(Duration::from_secs(30))
        );
        assert_eq!(
            scope
                .timeout(Some(Duration::from_secs(120)))
                .unwrap_err()
                .kind(),
            crate::error::ErrorKind::Forbidden
        );
    }

    #[test]
    fn the_output_limit_is_the_stricter_of_the_two() {
        let scope = Scope::parse(&serde_json::json!({"max_output_bytes": 1024})).unwrap();
        assert_eq!(scope.output_limit(None), Some(1024));
        assert_eq!(scope.output_limit(Some(2048)), Some(1024));
        assert_eq!(scope.output_limit(Some(16)), Some(16));
    }

    #[test]
    fn unknown_fields_are_rejected() {
        assert!(Scope::parse(&serde_json::json!({"commands": [], "root": "/"})).is_err());
    }
}
