//! The fields every record of one process carries.
//!
//! A record answers two different questions: "what happened" (event fields) and
//! "whose process said so" (resource fields). Putting the second group here
//! means a host cannot forget one at a single call site, and the same values
//! end up on stdout, in the file, and in the support bundle.
//!
//! The install identity itself is *not* created here: it belongs to
//! `flowcraft_kernel::identity`, and the pipeline only renders what it is
//! given. That keeps provisioning out of the telemetry crate.

use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{SystemTime, UNIX_EPOCH};

use crate::attrs::attr;

/// A resource field's value.
///
/// Typed, because a counter rendered as `"1"` cannot be compared against `1` in
/// a query, and the install epoch is exactly the kind of field that gets
/// compared.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Value {
    /// Free text.
    Text(String),
    /// A non-negative integer.
    Count(u64),
}

impl Value {
    /// The value as text, for the human-readable sink and for tests.
    #[must_use]
    pub fn as_text(&self) -> String {
        match self {
            Self::Text(text) => text.clone(),
            Self::Count(number) => number.to_string(),
        }
    }
}

/// Process-scoped fields attached to every record.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Resource {
    /// `service.name`: the binary's logical name, e.g. `flowcraftd`.
    pub service: String,
    /// `service.version`.
    pub version: String,
    /// The application embedding the platform, when it is not the platform.
    pub app: Option<String>,
    /// Machine- or user-scoped install identifier.
    pub install: Option<String>,
    /// How `install` was obtained: `machine`, `user`, or `ephemeral`.
    pub install_scope: Option<String>,
    /// Bumped whenever the install identity is reset.
    pub install_epoch: u32,
    /// The daemon run, unique per process start.
    pub session: String,
    /// A stable, non-reversible marker for the local user.
    pub principal: Option<String>,
}

impl Default for Resource {
    fn default() -> Self {
        Self {
            service: "flowcraft".to_owned(),
            version: env!("CARGO_PKG_VERSION").to_owned(),
            app: None,
            install: None,
            install_scope: None,
            install_epoch: 0,
            session: session_id(),
            principal: None,
        }
    }
}

impl Resource {
    /// The fields, in the order they are rendered.
    ///
    /// Empty values are skipped so a record never carries `flowcraft.install=""`.
    #[must_use]
    pub fn fields(&self) -> Vec<(&'static str, Value)> {
        let mut fields = Vec::with_capacity(7);
        if !self.service.is_empty() {
            fields.push(("service.name", Value::Text(self.service.clone())));
        }
        if !self.version.is_empty() {
            fields.push(("service.version", Value::Text(self.version.clone())));
        }
        if let Some(app) = self.app.as_deref().filter(|value| !value.is_empty()) {
            fields.push((attr::APP, Value::Text(app.to_owned())));
        }
        if let Some(install) = self.install.as_deref().filter(|value| !value.is_empty()) {
            fields.push((attr::INSTALL, Value::Text(install.to_owned())));
        }
        if let Some(scope) = self
            .install_scope
            .as_deref()
            .filter(|value| !value.is_empty())
        {
            fields.push((attr::INSTALL_SCOPE, Value::Text(scope.to_owned())));
            fields.push((
                attr::INSTALL_EPOCH,
                Value::Count(u64::from(self.install_epoch)),
            ));
        }
        if !self.session.is_empty() {
            fields.push((attr::SESSION, Value::Text(self.session.clone())));
        }
        if let Some(principal) = self.principal.as_deref().filter(|value| !value.is_empty()) {
            fields.push((attr::PRINCIPAL, Value::Text(principal.to_owned())));
        }
        fields
    }
}

/// A session identifier for one process run.
///
/// Not a security token: it only has to be unique among the runs of one machine
/// and stable for the life of the process, so the process id plus a nanosecond
/// clock is enough and costs no dependency.
#[must_use]
pub fn session_id() -> String {
    static COUNTER: AtomicU64 = AtomicU64::new(0);
    let nanos = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|duration| duration.as_nanos() as u64)
        .unwrap_or(0);
    let salt = COUNTER.fetch_add(1, Ordering::Relaxed);
    format!(
        "s-{:08x}{:08x}{:04x}",
        std::process::id(),
        nanos as u32,
        salt as u16
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn empty_values_are_not_rendered() {
        let resource = Resource {
            service: String::new(),
            version: String::new(),
            ..Resource::default()
        };
        let keys: Vec<&str> = resource.fields().into_iter().map(|(key, _)| key).collect();
        assert!(!keys.contains(&"service.name"));
        assert!(!keys.contains(&attr::INSTALL));
        assert!(keys.contains(&attr::SESSION));
    }

    #[test]
    fn epoch_only_travels_with_a_scope() {
        let resource = Resource {
            install: Some("01ABC".to_owned()),
            ..Resource::default()
        };
        let keys: Vec<&str> = resource.fields().into_iter().map(|(key, _)| key).collect();
        assert!(keys.contains(&attr::INSTALL));
        assert!(!keys.contains(&attr::INSTALL_EPOCH));

        let resource = Resource {
            install_scope: Some("machine".to_owned()),
            install_epoch: 2,
            ..resource
        };
        let fields = resource.fields();
        let epoch = fields
            .iter()
            .find(|(key, _)| *key == attr::INSTALL_EPOCH)
            .map(|(_, value)| value.clone());
        assert_eq!(epoch, Some(Value::Count(2)));
    }

    #[test]
    fn session_ids_differ_between_calls() {
        assert_ne!(session_id(), session_id());
    }
}
