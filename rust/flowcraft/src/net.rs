//! Remote-target rules, shared by everything that dials one.
//!
//! The rule was written in `kernel::config::validate` — "a `dns` target requires
//! TLS" — and that was the whole problem: it is enforced only for `platform.yaml`,
//! while capability settings are deserialized straight into a provider. So a
//! `tool.Source/mcp` url could be `http://…` with nothing to stop it (`S7`), not
//! because anyone decided plaintext was fine, but because the rule lived in one
//! place that the other caller never visits.
//!
//! It sits at the crate root rather than in `kernel` for that reason: a provider
//! shape (`--no-default-features --features tool`) does not compile the kernel, and
//! a rule that only the kernel can reach is the bug this module exists to fix.
//!
//! From here on the rule is one function, and both surfaces call it.

use std::borrow::Cow;

use crate::error::{Error, Result};

/// A remote target must be encrypted, unless it is loopback and the caller opted
/// in.
///
/// `allow_insecure` is the opt-in and belongs to the *capability's* settings, not
/// to this function: a loopback target is only as private as the network
/// namespace it is in, which this layer cannot see.
///
/// # Errors
///
/// A plaintext scheme is `Validation`; an unparseable or unsupported one is
/// `Validation` too, with the offending value quoted so a config typo is
/// diagnosable.
pub fn require_secure_target(target: &str, allow_insecure: bool) -> Result<()> {
    let target = target.trim();
    let Some((scheme, rest)) = target.split_once("://") else {
        return Err(Error::validation(format!(
            "remote target {target:?} has no scheme (expected https:// or wss://)"
        )));
    };
    let scheme = scheme.to_ascii_lowercase();
    if matches!(scheme.as_str(), "https" | "wss") {
        return Ok(());
    }
    if !matches!(scheme.as_str(), "http" | "ws") {
        return Err(Error::validation(format!(
            "remote target {target:?} uses {scheme}://, which this build does not speak"
        )));
    }
    // `allow_insecure` alone is not enough: it opens a plaintext target, so it is
    // only honoured for a host that cannot leave the machine.
    if allow_insecure && is_loopback_target(rest) {
        return Ok(());
    }
    if allow_insecure {
        return Err(Error::validation(format!(
            "remote target {target:?} is plaintext; allow_insecure only applies to a loopback host"
        )));
    }
    Err(Error::validation(format!(
        "remote target {target:?} requires tls (use {scheme}s://, or set allow_insecure for a loopback host)"
    )))
}

/// Whether the authority of a URL points at this machine.
///
/// Deliberately literal: `localhost`, `127.0.0.0/8`, `[::1]`, and `0:0:0:0:0:0:0:1`.
/// A name that *resolves* to loopback is not accepted, because a resolver is not
/// a trust boundary (and DNS-rebinding a name that resolves to loopback into one
/// that does not is exactly the trick this has to refuse).
#[must_use]
pub fn is_loopback_target(authority: &str) -> bool {
    // Strip userinfo, then the port, then any IPv6 brackets.
    let authority = authority.split('/').next().unwrap_or(authority);
    let authority = authority
        .rsplit_once('@')
        .map_or(authority, |(_, rest)| rest);
    let host: Cow<'_, str> = if let Some(rest) = authority.strip_prefix('[') {
        match rest.split_once(']') {
            Some((host, _)) => Cow::Borrowed(host),
            None => return false,
        }
    } else {
        match authority.rsplit_once(':') {
            Some((host, port)) if port.chars().all(|c| c.is_ascii_digit()) => Cow::Borrowed(host),
            _ => Cow::Borrowed(authority),
        }
    };
    if host.eq_ignore_ascii_case("localhost") {
        return true;
    }
    match host.parse::<std::net::IpAddr>() {
        Ok(ip) => ip.is_loopback(),
        Err(_) => false,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn an_encrypted_target_is_accepted_and_needs_no_opt_in() {
        for target in [
            "https://api.example.com/v1",
            "wss://api.example.com/socket",
            "HTTPS://API.EXAMPLE.COM",
            "https://127.0.0.1:8443",
        ] {
            require_secure_target(target, false).expect(target);
        }
    }

    #[test]
    fn a_plaintext_target_is_refused_unless_it_is_loopback_and_opted_in() {
        let error = require_secure_target("http://api.example.com", false).expect_err("refused");
        assert!(error.to_string().contains("requires tls"), "{error}");

        let error = require_secure_target("http://api.example.com", true).expect_err("refused");
        assert!(
            error
                .to_string()
                .contains("only applies to a loopback host"),
            "{error}"
        );

        for target in [
            "http://127.0.0.1:8080",
            "http://localhost:8080/health",
            "http://[::1]:8080",
            "ws://127.0.0.1:9000",
            "http://user:pass@localhost:1/x",
        ] {
            require_secure_target(target, true).expect(target);
            // The same target without the opt-in is still refused.
            require_secure_target(target, false).expect_err(target);
        }
    }

    #[test]
    fn a_name_that_only_resolves_to_loopback_is_not_loopback() {
        // `.localhost` and a hosts entry are resolver facts, not syntax.
        assert!(!is_loopback_target("api.localhost:80"));
        assert!(!is_loopback_target("127.0.0.1.example.com:80"));
        assert!(is_loopback_target("127.0.0.1:80"));
        assert!(is_loopback_target("127.1.2.3"));
        assert!(is_loopback_target("[::1]"));
        assert!(is_loopback_target("LOCALHOST"));
    }

    #[test]
    fn a_missing_or_foreign_scheme_is_reported() {
        let error = require_secure_target("api.example.com:443", false).expect_err("no scheme");
        assert!(error.to_string().contains("no scheme"), "{error}");
        let error = require_secure_target("ftp://api.example.com", false).expect_err("scheme");
        assert!(error.to_string().contains("does not speak"), "{error}");
    }
}
