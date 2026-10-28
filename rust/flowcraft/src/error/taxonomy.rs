//! Error taxonomy.
//!
//! Fourteen categories. Thirteen mirror the Go side's `core/errdefs`; [`Closed`]
//! is the one addition, and it exists because the Go tree spells a closed
//! handle with a bare `codes.FailedPrecondition` instead of a class of its own.
//! [`Error`] renders as its message alone so a diff against the Go side compares
//! like with like.
//!
//! Two things cross a process boundary: the category and, optionally, the
//! structured [`Detail`] list. The category used to survive only as a gRPC
//! status code, which is lossy — `BudgetExceeded` and `RateLimit` share
//! `ResourceExhausted`, `PolicyDenied` and `Forbidden` share
//! `PermissionDenied` — so a peer reading only the code downgraded a permanent
//! budget failure into a retryable limit and a policy refusal into a permission
//! problem. The category is therefore carried in `flowcraft.error.v1` on the
//! wire, and the status code stays a coarse hint for generic middleware.
//!
//! [`Closed`]: ErrorKind::Closed
//! [`Detail`]: crate::error::Detail

use std::fmt;

use crate::error::detail::{Detail, Retry};

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
    /// Every category, in declaration order.
    ///
    /// A new variant must be added here: the conformance tests iterate this
    /// list, so a category that never reaches the wire cannot hide.
    pub const ALL: [Self; 14] = [
        Self::Validation,
        Self::NotFound,
        Self::Unauthorized,
        Self::Forbidden,
        Self::Conflict,
        Self::RateLimit,
        Self::Timeout,
        Self::Interrupted,
        Self::Aborted,
        Self::Closed,
        Self::NotAvailable,
        Self::Internal,
        Self::BudgetExceeded,
        Self::PolicyDenied,
    ];

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

/// The classifiable part that only some errors carry, boxed so that
/// `Result<T, Error>` stays small.
///
/// `Error` is the return type of most fallible functions in the workspace, so
/// its size is paid on every stack frame, while details, retry metadata, and a
/// correlation id are only present on the cold path. One nullable pointer keeps
/// the common case cheap and the lint that catches oversized `Err` variants
/// (`clippy::result_large_err`) quiet by construction rather than by allowance.
#[derive(Debug, Default)]
struct Context {
    details: Vec<Detail>,
    retry: Option<Retry>,
    request_id: Option<String>,
}

/// A classified platform error.
///
/// `Display` renders the message only; use [`Error::kind`] for the category.
/// That keeps a message diff against the Go side meaningful instead of
/// prefixing every line with a kind.
///
/// The source is diagnostic data, not redacted output: it may carry prompts,
/// credentials, or wire payloads, so it stays local and never serializes.
/// [`Error::details`] is the part that is safe to send.
#[derive(Debug)]
pub struct Error {
    kind: ErrorKind,
    message: String,
    context: Option<Box<Context>>,
    source: Option<Box<dyn std::error::Error + Send + Sync + 'static>>,
}

impl Error {
    /// Build an error with a category and a message.
    pub fn new(kind: ErrorKind, message: impl Into<String>) -> Self {
        Self {
            kind,
            message: message.into(),
            context: None,
            source: None,
        }
    }

    /// The detail context, allocated on first use.
    fn context_mut(&mut self) -> &mut Context {
        self.context
            .get_or_insert_with(|| Box::new(Context::default()))
    }

    /// Build an error whose category and domain code are given together.
    ///
    /// The convenience that keeps call sites short: `message` becomes both the
    /// top-level text (what a peer that ignores details sees) and the detail's
    /// own text (what a peer that keys on the code sees).
    ///
    /// ```
    /// use flowcraft::error::{Error, ErrorKind};
    ///
    /// let error = Error::coded(
    ///     ErrorKind::Forbidden,
    ///     "workspace",
    ///     "path_traversal",
    ///     "path escapes the view root",
    /// );
    /// assert_eq!(error.code(), Some("path_traversal"));
    /// assert_eq!(error.to_string(), "path escapes the view root");
    /// ```
    #[must_use]
    pub fn coded(
        kind: ErrorKind,
        domain: impl Into<String>,
        code: impl Into<String>,
        message: impl Into<String>,
    ) -> Self {
        let message = message.into();
        Self::new(kind, message.clone())
            .with_detail(Detail::new(domain, code).with_message(message))
    }

    /// Attach one domain-scoped detail.
    #[must_use]
    pub fn with_detail(mut self, detail: Detail) -> Self {
        self.context_mut().details.push(detail);
        self
    }

    /// Attach the peer's backoff hint and wire accounting.
    #[must_use]
    pub fn with_retry(mut self, retry: Retry) -> Self {
        self.context_mut().retry = Some(retry);
        self
    }

    /// Attach a peer-assigned correlation id.
    #[must_use]
    pub fn with_request_id(mut self, request_id: impl Into<String>) -> Self {
        self.context_mut().request_id = Some(request_id.into());
        self
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

    /// Every domain-scoped detail, in the order the layers produced them.
    #[must_use]
    pub fn details(&self) -> &[Detail] {
        self.context
            .as_deref()
            .map_or(&[][..], |context| context.details.as_slice())
    }

    /// The first detail owned by `domain`.
    #[must_use]
    pub fn detail(&self, domain: &str) -> Option<&Detail> {
        self.details()
            .iter()
            .find(|detail| detail.domain() == domain)
    }

    /// The first detail's code, when the error carries one.
    ///
    /// A single `(domain, code)` pair is by far the common shape, so this is the
    /// shortcut call sites and peers reach for; use [`Error::details`] when the
    /// domain matters.
    #[must_use]
    pub fn code(&self) -> Option<&str> {
        self.details().first().map(Detail::code)
    }

    /// The peer's backoff hint and wire accounting, when it reported them.
    #[must_use]
    pub fn retry(&self) -> Option<Retry> {
        self.context.as_deref().and_then(|context| context.retry)
    }

    /// The peer-assigned correlation id, when it reported one.
    #[must_use]
    pub fn request_id(&self) -> Option<&str> {
        self.context
            .as_deref()
            .and_then(|context| context.request_id.as_deref())
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
    use std::time::Duration;

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
        let mut names: Vec<&str> = ErrorKind::ALL.iter().map(|kind| kind.as_str()).collect();
        names.sort_unstable();
        names.dedup();
        assert_eq!(names.len(), ErrorKind::ALL.len());
    }

    #[test]
    fn coded_fills_the_message_on_both_layers() {
        let error = Error::coded(
            ErrorKind::Forbidden,
            "workspace",
            "path_traversal",
            "escapes the root",
        );
        assert_eq!(error.kind(), ErrorKind::Forbidden);
        assert_eq!(error.message(), "escapes the root");
        assert_eq!(error.code(), Some("path_traversal"));
        let detail = error.detail("workspace").expect("detail");
        assert_eq!(detail.message(), "escapes the root");
    }

    #[test]
    fn a_plain_error_carries_no_wire_detail() {
        let error = Error::validation("bad");
        assert_eq!(error.code(), None);
        assert_eq!(error.details(), &[]);
        assert!(error.retry().is_none());
        assert!(error.request_id().is_none());
    }

    #[test]
    fn detail_lookup_is_by_domain() {
        let error = Error::validation("bad")
            .with_detail(Detail::new("workspace", "path_empty"))
            .with_detail(Detail::new("inference", "invalid_request"));
        assert_eq!(error.code(), Some("path_empty"));
        assert_eq!(
            error.detail("inference").expect("detail").code(),
            "invalid_request"
        );
        assert!(error.detail("sandbox").is_none());
    }

    #[test]
    fn retry_and_request_id_are_opt_in() {
        let error = Error::not_available("later")
            .with_retry(Retry::after(Duration::from_secs(2)).with_attempts(3))
            .with_request_id("req-1");
        assert_eq!(
            error.retry().and_then(|r| r.backoff()),
            Some(Duration::from_secs(2))
        );
        assert_eq!(error.retry().map(|r| r.wire_attempts()), Some(3));
        assert_eq!(error.request_id(), Some("req-1"));
    }

    /// `Error` is the `Err` arm of nearly every function in the workspace, so its
    /// size is paid on every frame. Details, retry metadata, and a correlation id
    /// sit behind one pointer for that reason; if this grows, put the new field
    /// in `Context` instead of widening the error.
    #[test]
    fn the_error_stays_small() {
        assert!(
            std::mem::size_of::<Error>() <= 64,
            "Error grew to {} bytes",
            std::mem::size_of::<Error>()
        );
        assert!(std::mem::size_of::<std::result::Result<(), Error>>() <= 64);
    }

    #[test]
    fn io_errors_map_to_categories() {
        use std::error::Error as StdError;

        let error: Error = std::io::Error::from(std::io::ErrorKind::NotFound).into();
        assert_eq!(error.kind(), ErrorKind::NotFound);
        assert!(StdError::source(&error).is_some());
    }
}
