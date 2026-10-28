//! The tool codes a peer can act on.
//!
//! The tool path has more distinguishable refusals than any other capability: an unknown
//! name, a name declared twice, arguments that do not match the schema, a policy that said
//! no, a deadline, and a registry that is closing. Most of them arrive as
//! [`ErrorKind::Validation`] or [`ErrorKind::NotAvailable`], and a caller that conflates
//! them retries a refusal, or reports an operator's duplicate as a caller's mistake.
//!
//! The pair `(tool, code)` is the machine-readable key. Codes are **added, never
//! renamed**: a rename is a new code, because a peer keys behavior off the spelling.
//!
//! A policy refusal is deliberately absent: the middleware chain answers it with a tool
//! *result*, so it never becomes an [`Error`] and there is nothing for a code to key off.

use std::fmt;

use crate::error::{Error, ErrorKind};

/// The domain every code in this module belongs to.
pub const DOMAIN: &str = "tool";

/// A tool failure the caller can distinguish without parsing a message.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
#[non_exhaustive]
pub enum Code {
    /// The name is empty or has surrounding whitespace.
    NameInvalid,
    /// Two definitions claim one name.
    Duplicate,
    /// The name is well-formed but nothing is registered under it.
    Unknown,
    /// The arguments are not the shape the tool declared.
    Arguments,
    /// The call ran out of its deadline.
    Timeout,
    /// The registry is closed or gone; the caller needs a new one.
    RegistryClosed,
}

impl Code {
    /// Every code, in declaration order.
    ///
    /// A new code must be added here: the snapshot test iterates this list, so a spelling
    /// that never reaches a peer cannot hide.
    pub const ALL: [Self; 6] = [
        Self::NameInvalid,
        Self::Duplicate,
        Self::Unknown,
        Self::Arguments,
        Self::Timeout,
        Self::RegistryClosed,
    ];

    /// The stable wire spelling.
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::NameInvalid => "name_invalid",
            Self::Duplicate => "duplicate",
            Self::Unknown => "unknown",
            Self::Arguments => "arguments",
            Self::Timeout => "timeout",
            Self::RegistryClosed => "registry_closed",
        }
    }
}

impl fmt::Display for Code {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// A malformed request: the caller has to change the input.
pub(crate) fn validation(code: Code, message: impl Into<String>) -> Error {
    Error::coded(ErrorKind::Validation, DOMAIN, code.as_str(), message)
}

/// Two definitions, one name: the deployment has to change.
pub(crate) fn conflict(code: Code, message: impl Into<String>) -> Error {
    Error::coded(ErrorKind::Conflict, DOMAIN, code.as_str(), message)
}

/// Nothing here under that name.
pub(crate) fn not_found(code: Code, message: impl Into<String>) -> Error {
    Error::coded(ErrorKind::NotFound, DOMAIN, code.as_str(), message)
}

/// The deadline passed.
pub(crate) fn timeout(code: Code, message: impl Into<String>) -> Error {
    Error::coded(ErrorKind::Timeout, DOMAIN, code.as_str(), message)
}

/// The registry cannot serve now; a caller has to rebuild.
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
            [
                "name_invalid",
                "duplicate",
                "unknown",
                "arguments",
                "timeout",
                "registry_closed",
            ]
        );
        let mut unique = spellings.clone();
        unique.sort_unstable();
        unique.dedup();
        assert_eq!(unique.len(), spellings.len());
    }

    /// The point of the module: one kind, two codes. A duplicate is the deployment's and an
    /// unknown name is the caller's, and both are `Conflict`/`NotFound` only by accident.
    #[test]
    fn the_constructors_stamp_domain_and_code() {
        let error = conflict(Code::Duplicate, "two tools are called search");
        assert_eq!(error.kind(), ErrorKind::Conflict);
        let detail = error.detail(DOMAIN).expect("detail");
        assert_eq!(detail.code(), "duplicate");

        let error = not_found(Code::Unknown, "no such tool");
        assert_eq!(error.kind(), ErrorKind::NotFound);
        assert_eq!(error.code(), Some("unknown"));
    }
}
