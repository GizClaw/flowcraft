//! Error taxonomy.
//!
//! Fourteen categories. Thirteen mirror the Go side's `core/errdefs`; [`Closed`]
//! is the one addition, and it exists because the Go tree spells a closed
//! handle with a bare `codes.FailedPrecondition` instead of a class of its own.
//! The category is part of the cross-language contract because every transport
//! maps it to a status code, while the message is free-form. [`Error`] renders
//! as its message alone so a diff against the Go side compares like with like.
//!
//! [`Closed`]: ErrorKind::Closed

use std::fmt;

/// Convenience alias for results carrying a classified [`Error`].
pub type Result<T> = std::result::Result<T, Error>;

/// The classification of an error.
///
/// `#[non_exhaustive]`: adding a category is a contract change, and downstream
/// `match` must be forced to acknowledge it.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
#[non_exhaustive]
pub enum ErrorKind {
    /// The request is malformed, or a document violates its schema.
    Validation,
    /// The addressed thing does not exist.
    NotFound,
    /// Authentication is missing or invalid.
    Unauthorized,
    /// Authentication succeeded but the action is not allowed.
    Forbidden,
    /// The action conflicts with current state.
    Conflict,
    /// A rate limit was hit.
    RateLimit,
    /// A deadline elapsed.
    Timeout,
    /// Work was interrupted (cancellation, shutdown).
    Interrupted,
    /// Work was aborted by a policy decision inside the platform.
    Aborted,
    /// The handle was closed; the operation cannot be performed on it anymore.
    ///
    /// This is a lifecycle statement, not a policy refusal and not a failure of
    /// the request: retrying it unchanged never helps, and a caller that sees it
    /// has to obtain a new handle (a new session, a new journal reader).
    ///
    /// Unlike the other categories it has no `core/errdefs` counterpart: the Go
    /// sandbox maps its own closed-handle sentinel to `FAILED_PRECONDITION`
    /// directly, so the wire still agrees.
    Closed,
    /// The capability exists but is not usable right now.
    NotAvailable,
    /// An invariant broke; a bug.
    Internal,
    /// A budget or quota was exhausted.
    BudgetExceeded,
    /// A policy engine denied the action.
    PolicyDenied,
}

impl ErrorKind {
    /// The stable wire spelling used in conformance fixtures.
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Validation => "validation",
            Self::NotFound => "not_found",
            Self::Unauthorized => "unauthorized",
            Self::Forbidden => "forbidden",
            Self::Conflict => "conflict",
            Self::RateLimit => "rate_limit",
            Self::Timeout => "timeout",
            Self::Interrupted => "interrupted",
            Self::Aborted => "aborted",
            Self::Closed => "closed",
            Self::NotAvailable => "not_available",
            Self::Internal => "internal",
            Self::BudgetExceeded => "budget_exceeded",
            Self::PolicyDenied => "policy_denied",
        }
    }

    /// The canonical gRPC status code for this category.
    ///
    /// Kept here, not in the transport crate, so every endpoint maps a category
    /// the same way and a client sees one status per failure class.
    #[must_use]
    pub const fn grpc_code(self) -> i32 {
        match self {
            Self::Validation => 3,     // InvalidArgument
            Self::NotFound => 5,       // NotFound
            Self::Unauthorized => 16,  // Unauthenticated
            Self::Forbidden => 7,      // PermissionDenied
            Self::Conflict => 6,       // AlreadyExists
            Self::RateLimit => 8,      // ResourceExhausted
            Self::Timeout => 4,        // DeadlineExceeded
            Self::Interrupted => 1,    // Cancelled
            Self::Aborted => 10,       // Aborted
            Self::Closed => 9,         // FailedPrecondition
            Self::NotAvailable => 14,  // Unavailable
            Self::Internal => 13,      // Internal
            Self::BudgetExceeded => 8, // ResourceExhausted
            Self::PolicyDenied => 7,   // PermissionDenied
        }
    }

    /// Whether a caller may retry the same operation unchanged.
    #[must_use]
    pub const fn is_retryable(self) -> bool {
        matches!(
            self,
            Self::RateLimit
                | Self::Timeout
                | Self::NotAvailable
                | Self::Interrupted
                | Self::Aborted
        )
    }
}

impl fmt::Display for ErrorKind {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// A classified platform error.
///
/// `Display` renders the message only; use [`Error::kind`] for the category.
/// That keeps a message diff against the Go side meaningful instead of
/// prefixing every line with a kind.
#[derive(Debug)]
pub struct Error {
    kind: ErrorKind,
    message: String,
    source: Option<Box<dyn std::error::Error + Send + Sync + 'static>>,
}

impl Error {
    /// Build an error with a category and a message.
    pub fn new(kind: ErrorKind, message: impl Into<String>) -> Self {
        Self {
            kind,
            message: message.into(),
            source: None,
        }
    }

    /// Attach a lower-level cause.
    #[must_use]
    pub fn with_source(mut self, source: impl std::error::Error + Send + Sync + 'static) -> Self {
        self.source = Some(Box::new(source));
        self
    }

    /// Attach a lower-level cause that is already boxed (e.g. a Boxed `Error`).
    #[must_use]
    pub fn with_boxed_source(
        mut self,
        source: Box<dyn std::error::Error + Send + Sync + 'static>,
    ) -> Self {
        self.source = Some(source);
        self
    }

    /// The category.
    #[must_use]
    pub fn kind(&self) -> ErrorKind {
        self.kind
    }

    /// The message without the category prefix.
    #[must_use]
    pub fn message(&self) -> &str {
        &self.message
    }

    /// `category: message`, for logs.
    #[must_use]
    pub fn to_classified_string(&self) -> String {
        format!("{}: {}", self.kind, self.message)
    }

    /// Malformed request or document.
    pub fn validation(message: impl Into<String>) -> Self {
        Self::new(ErrorKind::Validation, message)
    }

    /// Missing thing.
    pub fn not_found(message: impl Into<String>) -> Self {
        Self::new(ErrorKind::NotFound, message)
    }

    /// Authentication failure.
    pub fn unauthorized(message: impl Into<String>) -> Self {
        Self::new(ErrorKind::Unauthorized, message)
    }

    /// Authorized but denied.
    pub fn forbidden(message: impl Into<String>) -> Self {
        Self::new(ErrorKind::Forbidden, message)
    }

    /// State conflict.
    pub fn conflict(message: impl Into<String>) -> Self {
        Self::new(ErrorKind::Conflict, message)
    }

    /// Rate limited.
    pub fn rate_limit(message: impl Into<String>) -> Self {
        Self::new(ErrorKind::RateLimit, message)
    }

    /// Deadline elapsed.
    pub fn timeout(message: impl Into<String>) -> Self {
        Self::new(ErrorKind::Timeout, message)
    }

    /// Cancelled.
    pub fn interrupted(message: impl Into<String>) -> Self {
        Self::new(ErrorKind::Interrupted, message)
    }

    /// Aborted by policy.
    pub fn aborted(message: impl Into<String>) -> Self {
        Self::new(ErrorKind::Aborted, message)
    }

    /// The handle was closed.
    pub fn closed(message: impl Into<String>) -> Self {
        Self::new(ErrorKind::Closed, message)
    }

    /// Capability present but unusable now.
    pub fn not_available(message: impl Into<String>) -> Self {
        Self::new(ErrorKind::NotAvailable, message)
    }

    /// Invariant broken.
    pub fn internal(message: impl Into<String>) -> Self {
        Self::new(ErrorKind::Internal, message)
    }

    /// Budget exhausted.
    pub fn budget_exceeded(message: impl Into<String>) -> Self {
        Self::new(ErrorKind::BudgetExceeded, message)
    }

    /// Denied by policy.
    pub fn policy_denied(message: impl Into<String>) -> Self {
        Self::new(ErrorKind::PolicyDenied, message)
    }
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.message)
    }
}

impl std::error::Error for Error {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        self.source
            .as_deref()
            .map(|source| source as &(dyn std::error::Error + 'static))
    }
}

impl From<std::io::Error> for Error {
    fn from(error: std::io::Error) -> Self {
        let kind = match error.kind() {
            std::io::ErrorKind::NotFound => ErrorKind::NotFound,
            std::io::ErrorKind::PermissionDenied => ErrorKind::Forbidden,
            std::io::ErrorKind::AlreadyExists => ErrorKind::Conflict,
            std::io::ErrorKind::TimedOut => ErrorKind::Timeout,
            std::io::ErrorKind::InvalidInput => ErrorKind::Validation,
            std::io::ErrorKind::Interrupted => ErrorKind::Interrupted,
            _ => ErrorKind::Internal,
        };
        Self::new(kind, error.to_string()).with_source(error)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn display_is_message_only() {
        let error = Error::validation("resources.ws: kind is empty");
        assert_eq!(error.to_string(), "resources.ws: kind is empty");
        assert_eq!(error.kind(), ErrorKind::Validation);
        assert_eq!(
            error.to_classified_string(),
            "validation: resources.ws: kind is empty"
        );
    }

    #[test]
    fn every_kind_has_a_distinct_wire_name() {
        let kinds = [
            ErrorKind::Validation,
            ErrorKind::NotFound,
            ErrorKind::Unauthorized,
            ErrorKind::Forbidden,
            ErrorKind::Conflict,
            ErrorKind::RateLimit,
            ErrorKind::Timeout,
            ErrorKind::Interrupted,
            ErrorKind::Aborted,
            ErrorKind::Closed,
            ErrorKind::NotAvailable,
            ErrorKind::Internal,
            ErrorKind::BudgetExceeded,
            ErrorKind::PolicyDenied,
        ];
        let mut names: Vec<&str> = kinds.iter().map(|kind| kind.as_str()).collect();
        names.sort_unstable();
        names.dedup();
        assert_eq!(names.len(), kinds.len());
    }

    #[test]
    fn io_errors_map_to_categories() {
        use std::error::Error as StdError;

        let error: Error = std::io::Error::from(std::io::ErrorKind::NotFound).into();
        assert_eq!(error.kind(), ErrorKind::NotFound);
        assert!(StdError::source(&error).is_some());
    }
}
