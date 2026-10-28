//! The secret codes a peer can act on.
//!
//! A category alone is too coarse to be useful: "the name is malformed", "the grant does
//! not cover it", and "the store cannot be opened" are three different fixes for the
//! operator and one [`ErrorKind`] for a caller. The pair `(secret, code)` is the
//! machine-readable key.
//!
//! Codes are **added, never renamed**: a rename is a new code, because a peer keys
//! behavior off the spelling.
//!
//! Two conditions deliberately carry no code, for the same reason a missing secret is not
//! an error at all:
//!
//! * a name the store simply does not have — `lookup` answers `Ok(None)`, and silence
//!   stays silence;
//! * a read-only store — the write side is an `Option<&dyn Mutable>`, so "I cannot write"
//!   is answered by the shape of the handle rather than by a failure.
//!
//! A store's own corruption (a key file that is not hexadecimal, a seal that does not
//! open) carries no code either: that is an operator's problem, it is [`ErrorKind::Internal`],
//! and the message already names the file. A code would invite a caller to branch on a
//! condition it cannot fix.

use std::fmt;

use crate::error::{Error, ErrorKind};

/// The domain every code in this module belongs to.
pub const DOMAIN: &str = "secret";

/// A secret failure the caller can distinguish without parsing a message.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
#[non_exhaustive]
pub enum Code {
    /// The name is empty, has surrounding whitespace, or cannot map to the store.
    NameInvalid,
    /// The name is outside the grant's declared set.
    ScopeDenied,
    /// The backend cannot be reached or opened right now.
    StoreUnavailable,
}

impl Code {
    /// Every code, in declaration order.
    ///
    /// A new code must be added here: the snapshot test iterates this list, so a spelling
    /// that never reaches a peer cannot hide.
    pub const ALL: [Self; 3] = [Self::NameInvalid, Self::ScopeDenied, Self::StoreUnavailable];

    /// The stable wire spelling.
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::NameInvalid => "name_invalid",
            Self::ScopeDenied => "scope_denied",
            Self::StoreUnavailable => "store_unavailable",
        }
    }
}

impl fmt::Display for Code {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// A malformed request: the caller has to change the name.
pub(crate) fn validation(code: Code, message: impl Into<String>) -> Error {
    Error::coded(ErrorKind::Validation, DOMAIN, code.as_str(), message)
}

/// A refusal: retrying it unchanged never helps.
///
/// Compiled with the wire only, because the grant scope is the one refusal of this kind and
/// it lives in the endpoint. A caller without a wire has no token to be outside of.
#[cfg(any(feature = "wire", test))]
pub(crate) fn forbidden(code: Code, message: impl Into<String>) -> Error {
    Error::coded(ErrorKind::Forbidden, DOMAIN, code.as_str(), message)
}

/// The store is unreachable now: the same call may work later.
pub(crate) fn not_available(code: Code, message: impl Into<String>) -> Error {
    Error::coded(ErrorKind::NotAvailable, DOMAIN, code.as_str(), message)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn codes_have_stable_distinct_spellings() {
        let spellings: Vec<&str> = Code::ALL.iter().map(|code| code.as_str()).collect();
        assert_eq!(
            spellings,
            ["name_invalid", "scope_denied", "store_unavailable"]
        );
        let mut unique = spellings.clone();
        unique.sort_unstable();
        unique.dedup();
        assert_eq!(unique.len(), spellings.len());
    }

    #[test]
    fn the_constructors_stamp_domain_and_code() {
        let error = forbidden(Code::ScopeDenied, "outside the grant");
        assert_eq!(error.kind(), ErrorKind::Forbidden);
        assert_eq!(error.to_string(), "outside the grant");
        let detail = error.detail(DOMAIN).expect("detail");
        assert_eq!(detail.code(), "scope_denied");
        assert_eq!(detail.message(), "outside the grant");

        let error = not_available(Code::StoreUnavailable, "the keychain is unusable");
        assert_eq!(error.kind(), ErrorKind::NotAvailable);
        assert_eq!(error.code(), Some("store_unavailable"));
    }
}
