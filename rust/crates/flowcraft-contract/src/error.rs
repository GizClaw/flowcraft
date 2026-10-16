//! Error classification: the kinds core/errdefs marks an error with, and what
//! each kind answers to.
//!
//! core classifies by marker interface: `errdefs.Validation(err)` wraps an
//! error, the wrapper's text *is* the wrapped error's text, and `IsValidation`
//! walks the chain looking for the marker. The port keeps both of those
//! facts -- classification lives on the chain, and wrapping never rewrites
//! text -- and states the kind as a value: [`Error`] carries an [`ErrorKind`]
//! beside the message it prints.
//!
//! Deliberately not here, and where each thing goes instead:
//!
//! - the `IsXxx` predicates: `kind_of(err) == Some(kind)` is the whole of
//!   them, and `match` on [`ErrorKind`] is what the compiler can check.
//! - `FromContext`: mapping a deadline or a cancellation onto a kind is a
//!   decision about the port's own cancellation types, so it belongs where
//!   those types are (the runtime slice), not in the contract.
//! - the retry hints (`WithRetryAfter`, `WithRetryCount`, `WithRequestID`):
//!   same shape of data on an error, but their call sites are the transports
//!   that read the headers, so they land with the drivers.
//! - `errors.Join`: Rust has no multi-error to walk. A chain is a list.
//!
//! `rust/conformance/error-contract.json` pins the parts that do exist,
//! against a dump of core's own answers.

use std::error::Error as StdError;
use std::fmt;

/// A classification an error can carry.
///
/// The variants are `core/errdefs`' marker interfaces, one for one, in the
/// order `HTTPStatus` tests them -- which is also the order
/// [`http_status`] scans a chain in. The order is load-bearing: a chain that
/// carries two kinds answers with the earlier one, regardless of depth.
///
/// `#[non_exhaustive]`: a new kind in core is not a breaking change here.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
#[non_exhaustive]
pub enum ErrorKind {
    /// Nothing at the address named.
    NotFound,
    /// The caller's input is wrong, and retrying it as-is cannot help.
    Validation,
    /// The credential was refused.
    Unauthorized,
    /// The request is not allowed (or the account is not payable).
    Forbidden,
    /// The resource is already in a competing state.
    Conflict,
    /// The upstream is rate-limiting this caller.
    RateLimit,
    /// A token, cost or count budget -- the caller's own -- is exhausted.
    BudgetExceeded,
    /// A policy layer (allow-list, egress filter, role check) refused.
    PolicyDenied,
    /// The attempt ran out of its deadline.
    Timeout,
    /// The upstream is transiently unusable; retrying may work.
    NotAvailable,
    /// The failure is ours, or unattributable.
    Internal,
    /// The work was interrupted (not by the caller's own decision).
    Interrupted,
    /// The caller aborted it (a cooperative stop, not a failure).
    Aborted,
}

/// The order `core/errdefs.HTTPStatus` tests categories in, which decides
/// which status a chain carrying more than one kind answers with. It is the
/// declaration order of [`ErrorKind`] because that keeps the two readable
/// side by side; the array below is what the code walks.
const HTTP_STATUS_PRIORITY: [ErrorKind; 13] = [
    ErrorKind::NotFound,
    ErrorKind::Validation,
    ErrorKind::Unauthorized,
    ErrorKind::Forbidden,
    ErrorKind::Conflict,
    ErrorKind::RateLimit,
    ErrorKind::BudgetExceeded,
    ErrorKind::PolicyDenied,
    ErrorKind::Timeout,
    ErrorKind::NotAvailable,
    ErrorKind::Internal,
    ErrorKind::Interrupted,
    ErrorKind::Aborted,
];

impl ErrorKind {
    /// The status core's `HTTPStatus` answers for an error whose only
    /// classification is this kind.
    ///
    /// For a chain that carries several, the chain's answer is
    /// [`http_status`]'s, not this method's: BudgetExceeded is 429 here and
    /// still 429 under an outer `Internal`, but a `Validation` deep inside a
    /// `RateLimit` answers 400, because Validation comes first in the scan.
    pub fn http_status(self) -> u16 {
        match self {
            ErrorKind::NotFound => 404,
            ErrorKind::Validation => 400,
            ErrorKind::Unauthorized => 401,
            ErrorKind::Forbidden => 403,
            ErrorKind::Conflict => 409,
            ErrorKind::RateLimit => 429,
            // Budget refusals and upstream rate limits share the wire shape;
            // operators tell them apart by `kind()`, not by the status.
            ErrorKind::BudgetExceeded => 429,
            // Same for policy denials and upstream 403s.
            ErrorKind::PolicyDenied => 403,
            ErrorKind::Timeout => 504,
            ErrorKind::NotAvailable => 503,
            ErrorKind::Internal => 500,
            ErrorKind::Interrupted | ErrorKind::Aborted => 409,
        }
    }
}

/// An error with a classification and the text core would print for it.
///
/// Construction is the only way in, which is the point: `message` is the text
/// a caller following core's rules would have ended up with, so the two
/// implementations print the same bytes for the same chain. The wrappers in
/// `provider` are transparent -- they pass the cause's text through -- so an
/// [`Error`] wrapping a cause prints exactly the cause's text, never a kind
/// prefix or a re-rendering.
#[derive(Debug)]
pub struct Error {
    kind: ErrorKind,
    message: String,
    source: Option<Box<dyn StdError + Send + Sync + 'static>>,
}

impl Error {
    /// A classified error with no cause underneath it.
    pub fn new(kind: ErrorKind, message: impl Into<String>) -> Self {
        Error {
            kind,
            message: message.into(),
            source: None,
        }
    }

    /// A classified error that keeps `source` on the chain.
    ///
    /// `message` is the caller's text for the whole thing; core's
    /// `fmt.Errorf("%s: %w", ...)`-style wrappers put a prefix there, its
    /// marker wrappers pass the cause's text through unchanged. Either way
    /// the cause stays reachable via [`StdError::source`], so callers can
    /// still find the provider's own error type or the sentinel they started
    /// from.
    pub fn with_source(
        kind: ErrorKind,
        message: impl Into<String>,
        source: Box<dyn StdError + Send + Sync + 'static>,
    ) -> Self {
        Error {
            kind,
            message: message.into(),
            source: Some(source),
        }
    }

    /// The classification.
    pub fn kind(&self) -> ErrorKind {
        self.kind
    }
}

impl fmt::Display for Error {
    /// The message, and nothing else: this is the text a byte-for-byte
    /// comparison against core's errors sees.
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.message)
    }
}

impl StdError for Error {
    fn source(&self) -> Option<&(dyn StdError + 'static)> {
        match &self.source {
            Some(source) => Some(&**source),
            None => None,
        }
    }
}

/// The classification the first error on the chain carries, outermost first.
///
/// This is `errors.As`'s direction: the nearest marker wins, and `None` means
/// nothing on the chain is classified -- the case core answers with
/// `IsInternal` being false everywhere and `HTTPStatus` falling through to
/// 500.
pub fn kind_of(error: &(dyn StdError + 'static)) -> Option<ErrorKind> {
    let mut current = Some(error);
    while let Some(err) = current {
        if let Some(ours) = err.downcast_ref::<Error>() {
            return Some(ours.kind());
        }
        current = err.source();
    }
    None
}

/// Whether anything on the chain carries a classification.
///
/// The port's `HasClassification`: the guard that keeps a second pass from
/// rewriting a classification that is already there (a `Timeout` must not
/// come back as `NotAvailable` just because it passed through a provider
/// classifier).
pub fn has_classification(error: &(dyn StdError + 'static)) -> bool {
    kind_of(error).is_some()
}

/// The HTTP status `core/errdefs.HTTPStatus` answers for this chain.
///
/// A faithful port of its scan, which is *not* "look at the outermost kind":
/// it tests one category at a time, in [`HTTP_STATUS_PRIORITY`] order, and a
/// category matches if any error on the chain carries it. So
/// `RateLimit(Validation(cause))` answers 400, not 429 -- Validation is
/// tested first and the inner error has it.
///
/// An unclassified chain is 500. (core's `HTTPStatus(nil) = 200` has no
/// counterpart here: a caller holding no error knows that without asking.)
pub fn http_status(error: &(dyn StdError + 'static)) -> u16 {
    for kind in HTTP_STATUS_PRIORITY {
        if chain_has(error, kind) {
            return kind.http_status();
        }
    }
    500
}

/// Whether any error on the chain carries `kind`. core's `IsXxx`, folded.
fn chain_has(error: &(dyn StdError + 'static), kind: ErrorKind) -> bool {
    let mut current = Some(error);
    while let Some(err) = current {
        if err
            .downcast_ref::<Error>()
            .is_some_and(|ours| ours.kind() == kind)
        {
            return true;
        }
        current = err.source();
    }
    false
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A foreign error with a message and an optional cause, so the walkers
    /// are exercised against chains that are not made of [`Error`]s.
    #[derive(Debug)]
    struct Plain {
        message: &'static str,
        source: Option<Box<dyn StdError + Send + Sync + 'static>>,
    }

    impl Plain {
        fn new(message: &'static str) -> Self {
            Plain {
                message,
                source: None,
            }
        }

        fn wrapping(message: &'static str, source: impl StdError + Send + Sync + 'static) -> Self {
            Plain {
                message,
                source: Some(Box::new(source)),
            }
        }
    }

    impl fmt::Display for Plain {
        fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
            f.write_str(self.message)
        }
    }

    impl StdError for Plain {
        fn source(&self) -> Option<&(dyn StdError + 'static)> {
            match &self.source {
                Some(source) => Some(&**source),
                None => None,
            }
        }
    }

    #[test]
    fn every_kind_answers_core_s_statuses() {
        // The table core/errdefs.HTTPStatus switches over, one row each.
        let table = [
            (ErrorKind::NotFound, 404),
            (ErrorKind::Validation, 400),
            (ErrorKind::Unauthorized, 401),
            (ErrorKind::Forbidden, 403),
            (ErrorKind::Conflict, 409),
            (ErrorKind::RateLimit, 429),
            (ErrorKind::BudgetExceeded, 429),
            (ErrorKind::PolicyDenied, 403),
            (ErrorKind::Timeout, 504),
            (ErrorKind::NotAvailable, 503),
            (ErrorKind::Internal, 500),
            (ErrorKind::Interrupted, 409),
            (ErrorKind::Aborted, 409),
        ];
        for (kind, status) in table {
            assert_eq!(kind.http_status(), status, "{kind:?}");
        }
    }

    #[test]
    fn wrapping_never_rewrites_the_text() {
        let cause = Plain::new("openai.generate: 30s");
        let wrapped = Error::with_source(ErrorKind::Timeout, cause.to_string(), Box::new(cause));
        assert_eq!(wrapped.to_string(), "openai.generate: 30s");
    }

    #[test]
    fn the_cause_stays_on_the_chain() {
        let wrapped = Error::with_source(
            ErrorKind::Validation,
            "openai: boom",
            Box::new(Plain::new("boom")),
        );
        let Some(cause) = wrapped.source() else {
            panic!("the cause should stay on the chain");
        };
        assert_eq!(cause.to_string(), "boom");
        assert!(cause.downcast_ref::<Plain>().is_some());
        assert!(wrapped.source().and_then(|c| c.source()).is_none());
    }

    #[test]
    fn kind_of_finds_the_nearest_classification() {
        let inner = Error::new(ErrorKind::NotFound, "inner");
        let outer = Plain::wrapping("outer", inner);
        assert_eq!(kind_of(&outer), Some(ErrorKind::NotFound));

        let classified = Error::with_source(ErrorKind::Timeout, "outer: inner", Box::new(outer));
        assert_eq!(kind_of(&classified), Some(ErrorKind::Timeout));
    }

    #[test]
    fn unclassified_chains_answer_none_and_500() {
        let plain = Plain::wrapping("outer", Plain::new("inner"));
        assert_eq!(kind_of(&plain), None);
        assert!(!has_classification(&plain));
        assert_eq!(http_status(&plain), 500);
    }

    #[test]
    fn http_status_scans_in_core_s_order_not_by_depth() {
        // Validation is tested before RateLimit, so the inner error decides.
        let inner = Error::new(ErrorKind::Validation, "input too long");
        let outer = Error::with_source(ErrorKind::RateLimit, "rate limited", Box::new(inner));
        assert_eq!(kind_of(&outer), Some(ErrorKind::RateLimit));
        assert_eq!(http_status(&outer), 400);

        // And the reverse shape answers the same way.
        let inner = Error::new(ErrorKind::NotFound, "gone");
        let outer = Error::with_source(ErrorKind::Validation, "bad request", Box::new(inner));
        assert_eq!(http_status(&outer), 404);
    }

    #[test]
    fn http_status_sees_through_foreign_wrappers() {
        let classified = Plain::wrapping("op: %w", Error::new(ErrorKind::BudgetExceeded, "cap"));
        assert_eq!(http_status(&classified), 429);
        assert!(has_classification(&classified));
    }
}
