//! Structured, domain-scoped error detail.
//!
//! A [`Detail`] is the machine-readable half of an error: the owning domain, a
//! stable code, and the bounded scalars a peer needs to act without parsing
//! prose. It exists because a category alone is too coarse (every capability
//! reports `Validation`), while a message alone is too loose (nothing to match
//! on, and nothing redacted by construction).
//!
//! Two rules make the shape safe to put on a wire:
//!
//! * **Codes are added, never renamed.** `(domain, code)` is a versioned key, so
//!   a peer can key behavior off it, and a rename is a new code rather than a
//!   silent behavior change.
//! * **Fields are bounded scalars.** There is deliberately no binary or opaque
//!   channel: model output, prompts, and credentials are not "detail", and an
//!   error that can carry them is a leak surface by construction.

use std::collections::BTreeMap;
use std::time::Duration;

/// One domain's own vocabulary for a failure.
///
/// ```
/// use flowcraft_error::Detail;
///
/// let detail = Detail::new("workspace", "path_traversal")
///     .with_message("path escapes the view root")
///     .with_field("path", "../etc/passwd");
/// assert_eq!(detail.domain(), "workspace");
/// assert_eq!(detail.code(), "path_traversal");
/// assert_eq!(detail.field("path"), Some("../etc/passwd"));
/// ```
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct Detail {
    domain: String,
    code: String,
    message: String,
    fields: BTreeMap<String, String>,
}

impl Detail {
    /// Start a detail for `(domain, code)`.
    ///
    /// Both are stable identifiers: `domain` is the owning subsystem (for
    /// example `"workspace"`), `code` is a snake_case name unique within that
    /// domain (for example `"path_traversal"`).
    #[must_use]
    pub fn new(domain: impl Into<String>, code: impl Into<String>) -> Self {
        let domain = domain.into();
        let code = code.into();
        debug_assert!(!domain.is_empty(), "detail domain must not be empty");
        debug_assert!(!code.is_empty(), "detail code must not be empty");
        Self {
            domain,
            code,
            message: String::new(),
            fields: BTreeMap::new(),
        }
    }

    /// Attach the human-readable explanation of this occurrence.
    #[must_use]
    pub fn with_message(mut self, message: impl Into<String>) -> Self {
        self.message = message.into();
        self
    }

    /// Attach one bounded scalar.
    #[must_use]
    pub fn with_field(mut self, key: impl Into<String>, value: impl Into<String>) -> Self {
        self.fields.insert(key.into(), value.into());
        self
    }

    /// Attach several bounded scalars at once.
    #[must_use]
    pub fn with_fields<I, K, V>(mut self, fields: I) -> Self
    where
        I: IntoIterator<Item = (K, V)>,
        K: Into<String>,
        V: Into<String>,
    {
        for (key, value) in fields {
            self.fields.insert(key.into(), value.into());
        }
        self
    }

    /// The owning capability or subsystem.
    #[must_use]
    pub fn domain(&self) -> &str {
        &self.domain
    }

    /// The stable code, unique within [`Detail::domain`].
    #[must_use]
    pub fn code(&self) -> &str {
        &self.code
    }

    /// The explanation of this occurrence; empty when the layer gave none.
    #[must_use]
    pub fn message(&self) -> &str {
        &self.message
    }

    /// Every bounded scalar, ordered by key.
    #[must_use]
    pub fn fields(&self) -> &BTreeMap<String, String> {
        &self.fields
    }

    /// One bounded scalar.
    #[must_use]
    pub fn field(&self, key: &str) -> Option<&str> {
        self.fields.get(key).map(String::as_str)
    }
}

/// Backoff hint and wire accounting reported by the failing layer.
///
/// Diagnostic metadata, not an instruction: it says what the layer observed, and
/// a caller still decides whether retrying is allowed at all.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct Retry {
    after: Option<Duration>,
    attempts: u32,
}

impl Retry {
    /// A backoff hint from the peer (for example a `Retry-After` header).
    #[must_use]
    pub fn after(after: Duration) -> Self {
        Self {
            after: Some(after),
            attempts: 0,
        }
    }

    /// The number of wire sends the failing layer made before giving up.
    #[must_use]
    pub fn attempts(attempts: u32) -> Self {
        Self {
            after: None,
            attempts,
        }
    }

    /// Add or replace the backoff hint.
    #[must_use]
    pub fn with_after(mut self, after: Duration) -> Self {
        self.after = Some(after);
        self
    }

    /// Add or replace the wire-attempt count.
    #[must_use]
    pub fn with_attempts(mut self, attempts: u32) -> Self {
        self.attempts = attempts;
        self
    }

    /// The backoff hint, when the peer reported one.
    #[must_use]
    pub fn backoff(&self) -> Option<Duration> {
        self.after
    }

    /// The wire-attempt count; zero means "not reported".
    #[must_use]
    pub fn wire_attempts(&self) -> u32 {
        self.attempts
    }

    /// Whether this carries nothing worth putting on the wire.
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.after.is_none() && self.attempts == 0
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn fields_are_ordered_and_addressable() {
        let detail = Detail::new("inference", "undefined_tool")
            .with_message("tool was never shown")
            .with_fields([("tool_name", "search"), ("field", "generate.context")]);
        assert_eq!(detail.field("tool_name"), Some("search"));
        assert_eq!(detail.field("missing"), None);
        let keys: Vec<&str> = detail.fields().keys().map(String::as_str).collect();
        assert_eq!(keys, ["field", "tool_name"]);
    }

    #[test]
    fn a_detail_without_a_message_renders_empty() {
        let detail = Detail::new("sandbox", "session_closed");
        assert_eq!(detail.code(), "session_closed");
        assert_eq!(detail.message(), "");
        assert!(detail.fields().is_empty());
    }

    #[test]
    fn retry_reports_only_what_it_knows() {
        let backoff = Retry::after(Duration::from_millis(1500));
        assert_eq!(backoff.backoff(), Some(Duration::from_millis(1500)));
        assert_eq!(backoff.wire_attempts(), 0);
        assert!(!backoff.is_empty());

        let attempts = Retry::attempts(3);
        assert_eq!(attempts.backoff(), None);
        assert_eq!(attempts.wire_attempts(), 3);

        assert!(Retry::default().is_empty());
    }
}
