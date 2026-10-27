//! The sandbox codes a peer can act on.
//!
//! Most sandbox refusals are [`ErrorKind::NotAvailable`]: the runner exists but
//! cannot honour this part of the request. A caller that cannot tell "this
//! backend has no TTY" from "this backend cannot enforce limits" cannot pick a
//! different backend for the request it actually has, so each refusal gets its
//! own code.
//!
//! The pair `(sandbox, code)` is the machine-readable key. Codes are **added,
//! never renamed**: a rename is a new code, because a peer keys behavior off the
//! spelling.
//!
//! Plan-time settings and poisoned locks stay uncoded on purpose: those are an
//! operator's problem, and the message already names the setting.

use std::fmt;

use crate::error::{Error, ErrorKind};

/// The domain every code in this module belongs to.
pub const DOMAIN: &str = "sandbox";

/// A sandbox failure the caller can distinguish without parsing a message.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
#[non_exhaustive]
pub enum Code {
    /// The session spec has no argv.
    ArgvEmpty,
    /// The runner cannot allocate a pseudo-terminal.
    TtyUnsupported,
    /// The runner cannot enforce the requested network mode.
    NetModeUnsupported,
    /// The runner cannot enforce a read-only filesystem.
    WriteModeUnsupported,
    /// The runner cannot enforce the requested cpu/memory/disk limits.
    LimitsUnsupported,
    /// The session was started without stdin, so it cannot be written to.
    StdinNotConfigured,
    /// The requested working directory does not exist.
    WorkdirMissing,
    /// A session with this id already exists.
    SessionExists,
    /// No session with this id.
    SessionNotFound,
    /// The program is outside the grant's command allow-list.
    CommandNotAllowed,
    /// The requested timeout is above the grant's ceiling.
    TimeoutExceedsGrant,
}

impl Code {
    /// Every code, in declaration order.
    ///
    /// A new code must be added here: the snapshot test iterates this list, so a
    /// spelling that never reaches a peer cannot hide.
    pub const ALL: [Self; 11] = [
        Self::ArgvEmpty,
        Self::TtyUnsupported,
        Self::NetModeUnsupported,
        Self::WriteModeUnsupported,
        Self::LimitsUnsupported,
        Self::StdinNotConfigured,
        Self::WorkdirMissing,
        Self::SessionExists,
        Self::SessionNotFound,
        Self::CommandNotAllowed,
        Self::TimeoutExceedsGrant,
    ];

    /// The stable wire spelling.
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::ArgvEmpty => "argv_empty",
            Self::TtyUnsupported => "tty_unsupported",
            Self::NetModeUnsupported => "net_mode_unsupported",
            Self::WriteModeUnsupported => "write_mode_unsupported",
            Self::LimitsUnsupported => "limits_unsupported",
            Self::StdinNotConfigured => "stdin_not_configured",
            Self::WorkdirMissing => "workdir_missing",
            Self::SessionExists => "session_exists",
            Self::SessionNotFound => "session_not_found",
            Self::CommandNotAllowed => "command_not_allowed",
            Self::TimeoutExceedsGrant => "timeout_exceeds_grant",
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

/// The capability is present but cannot serve this request as asked.
pub(crate) fn unavailable(code: Code, message: impl Into<String>) -> Error {
    Error::coded(ErrorKind::NotAvailable, DOMAIN, code.as_str(), message)
}

/// The addressed session or directory does not exist.
pub(crate) fn not_found(code: Code, message: impl Into<String>) -> Error {
    Error::coded(ErrorKind::NotFound, DOMAIN, code.as_str(), message)
}

/// The action conflicts with current state.
pub(crate) fn conflict(code: Code, message: impl Into<String>) -> Error {
    Error::coded(ErrorKind::Conflict, DOMAIN, code.as_str(), message)
}

/// The grant does not allow the action.
///
/// The one caller is the grant-scope check, which is a `wire` item. A
/// `sandbox`-without-`wire` build therefore has this function and no caller, by
/// design rather than by accident — the capability is a provider there, and a
/// provider has no grant scope to check.
#[cfg_attr(not(feature = "wire"), allow(dead_code))]
pub(crate) fn forbidden(code: Code, message: impl Into<String>) -> Error {
    Error::coded(ErrorKind::Forbidden, DOMAIN, code.as_str(), message)
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
                "argv_empty",
                "tty_unsupported",
                "net_mode_unsupported",
                "write_mode_unsupported",
                "limits_unsupported",
                "stdin_not_configured",
                "workdir_missing",
                "session_exists",
                "session_not_found",
                "command_not_allowed",
                "timeout_exceeds_grant",
            ]
        );
        let mut unique = spellings.clone();
        unique.sort_unstable();
        unique.dedup();
        assert_eq!(unique.len(), spellings.len());
    }

    #[test]
    fn the_constructors_stamp_domain_and_code() {
        let error = unavailable(Code::TtyUnsupported, "no TTY");
        assert_eq!(error.kind(), ErrorKind::NotAvailable);
        assert_eq!(error.to_string(), "no TTY");
        let detail = error.detail(DOMAIN).expect("detail");
        assert_eq!(detail.code(), "tty_unsupported");
        assert_eq!(error.code(), Some("tty_unsupported"));

        assert_eq!(
            not_found(Code::SessionNotFound, "gone").kind(),
            ErrorKind::NotFound
        );
        assert_eq!(
            conflict(Code::SessionExists, "taken").kind(),
            ErrorKind::Conflict
        );
        assert_eq!(
            forbidden(Code::CommandNotAllowed, "no").kind(),
            ErrorKind::Forbidden
        );
        assert_eq!(
            validation(Code::ArgvEmpty, "empty").kind(),
            ErrorKind::Validation
        );
    }
}
