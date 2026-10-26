//! The workspace codes a peer can act on.
//!
//! A category alone is too coarse to be useful: "path escapes the root",
//! "outside the grant scope", and "the policy said no" are all
//! [`ErrorKind::Forbidden`], yet a caller that mixes them up produces a
//! misleading operator report, and a caller that treats all three as retryable
//! retries a refusal forever.
//!
//! The pair `(workspace, code)` is the machine-readable key. Codes are **added,
//! never renamed**: a rename is a new code, because a peer keys behavior off the
//! spelling.
//!
//! Plan-time and invariant failures deliberately carry no code: a malformed
//! deployment document or a poisoned lock is an operator's problem, and the
//! message already says which setting or which lock.

use std::fmt;

use crate::error::{Error, ErrorKind};

/// The domain every code in this module belongs to.
pub const DOMAIN: &str = "workspace";

/// A workspace failure the caller can distinguish without parsing a message.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
#[non_exhaustive]
pub enum Code {
    /// The path climbs above the root of the view.
    PathTraversal,
    /// The path is absolute, or rooted in a form the platform refuses.
    PathAbsolute,
    /// The path exceeds the fixed length bound.
    PathTooLong,
    /// The grant scope allows reads but not writes.
    ScopeReadOnly,
    /// The path is outside the grant's declared root.
    ScopeDenied,
    /// A configured deny policy refused the access.
    AccessDenied,
    /// The call tried to remove the workspace or view root.
    RootRemoval,
    /// A payload exceeded the byte cap the caller asked for.
    TooLarge,
    /// The handle was closed; the caller has to obtain a new one.
    Closed,
}

impl Code {
    /// Every code, in declaration order.
    ///
    /// A new code must be added here: the snapshot test iterates this list, so a
    /// spelling that never reaches a peer cannot hide.
    pub const ALL: [Self; 9] = [
        Self::PathTraversal,
        Self::PathAbsolute,
        Self::PathTooLong,
        Self::ScopeReadOnly,
        Self::ScopeDenied,
        Self::AccessDenied,
        Self::RootRemoval,
        Self::TooLarge,
        Self::Closed,
    ];

    /// The stable wire spelling.
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::PathTraversal => "path_traversal",
            Self::PathAbsolute => "path_absolute",
            Self::PathTooLong => "path_too_long",
            Self::ScopeReadOnly => "scope_read_only",
            Self::ScopeDenied => "scope_denied",
            Self::AccessDenied => "access_denied",
            Self::RootRemoval => "root_removal",
            Self::TooLarge => "too_large",
            Self::Closed => "closed",
        }
    }
}

impl fmt::Display for Code {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// A refusal: the request itself is the problem, so retrying it unchanged never
/// helps.
pub(crate) fn forbidden(code: Code, message: impl Into<String>) -> Error {
    Error::coded(ErrorKind::Forbidden, DOMAIN, code.as_str(), message)
}

/// A malformed request: the caller has to change the input.
pub(crate) fn validation(code: Code, message: impl Into<String>) -> Error {
    Error::coded(ErrorKind::Validation, DOMAIN, code.as_str(), message)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn codes_have_stable_distinct_spellings() {
        let spellings: Vec<&str> = Code::ALL.iter().map(|code| code.as_str()).collect();
        assert_eq!(
            spellings,
            [
                "path_traversal",
                "path_absolute",
                "path_too_long",
                "scope_read_only",
                "scope_denied",
                "access_denied",
                "root_removal",
                "too_large",
                "closed",
            ]
        );
        let mut unique = spellings.clone();
        unique.sort_unstable();
        unique.dedup();
        assert_eq!(unique.len(), spellings.len());
    }

    #[test]
    fn the_constructors_stamp_domain_and_code() {
        let error = forbidden(Code::PathTraversal, "escapes the root");
        assert_eq!(error.kind(), ErrorKind::Forbidden);
        assert_eq!(error.to_string(), "escapes the root");
        let detail = error.detail(DOMAIN).expect("detail");
        assert_eq!(detail.code(), "path_traversal");
        assert_eq!(detail.message(), "escapes the root");

        let error = validation(Code::TooLarge, "over the cap");
        assert_eq!(error.kind(), ErrorKind::Validation);
        assert_eq!(error.code(), Some("too_large"));
    }
}
