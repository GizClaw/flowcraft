//! The policy types an egress decision is read from.
//!
//! These are core's `NetPolicy`, `NetRule`, `NetAction`, `NetMode` and
//! `MITMPolicy`, as data. [`Matcher`](super::matcher::Matcher) is the only
//! consumer in this slice, and it reads the rules through
//! [`NetPolicy::allow_hosts`] and [`NetPolicy::rules`].
//!
//! # Zero values
//!
//! Go lets a `NetPolicy{}` stand for "no policy", a `NetRule{}` for "deny the
//! empty host", and a `NetAction` of `0` for deny. Rust has no zero value, so
//! each type here implements [`Default`] with the same meaning: [`NetMode::Default`],
//! [`NetAction::Deny`], empty strings and vectors. A decoded but partial policy
//! therefore behaves the same on both sides.
//!
//! # What is not here
//!
//! The two `Validate` branches a Rust type cannot reach: an unknown
//! [`NetMode`] and an unknown [`NetAction`]. The enums have no such value, so
//! the checks that refuse them are always satisfied here and stay covered by
//! core's own tests.

use flowcraft_contract::error::{Error, ErrorKind};
use flowcraft_contract::text;

use super::url;

/// The outbound-network posture a sandbox enforces.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Hash)]
pub enum NetMode {
    /// Leave networking to the host.
    #[default]
    Default,
    /// Forbid outbound connections.
    DenyAll,
    /// Permit only the destinations the policy lists.
    AllowList,
    /// Route traffic through [`NetPolicy::proxy`].
    Proxy,
}

/// The verdict of one network rule.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Hash)]
pub enum NetAction {
    /// Refuse the destination.
    #[default]
    Deny,
    /// Permit the destination.
    Allow,
}

impl NetAction {
    /// `NetAction.String`: `"deny"` or `"allow"`.
    ///
    /// Core's method has a third answer, `"unknown"`, for a value outside the
    /// two constants. The variant that would produce it cannot be constructed.
    pub fn as_str(self) -> &'static str {
        match self {
            NetAction::Deny => "deny",
            NetAction::Allow => "allow",
        }
    }
}

impl std::fmt::Display for NetAction {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(self.as_str())
    }
}

/// One host-level allow or deny rule.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct NetRule {
    /// Whether the rule permits or refuses.
    pub action: NetAction,
    /// The host pattern, optionally with a `:port` suffix.
    pub host: String,
    /// The port the rule is restricted to, or `0` for any port.
    ///
    /// A `:port` suffix in [`NetRule::host`] overrides this.
    pub port: i32,
    /// A model-facing explanation, returned with a deny verdict and surfaced in
    /// proxy denial responses and audit records. Allow rules may carry one too;
    /// core returns it, and says in its docs that it is ignored.
    pub reason: String,
}

/// The outbound-networking posture of one sandbox.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct NetPolicy {
    /// Which posture to enforce.
    pub mode: NetMode,
    /// The legacy allow-list: hostname suffixes and exact IP literals, with the
    /// same optional `:port` suffix. Compiled as allow rules *after*
    /// [`NetPolicy::rules`], so an explicit deny always wins over it.
    pub allow_hosts: Vec<String>,
    /// The rules, in the order the configuration lists them.
    pub rules: Vec<NetRule>,
    /// The proxy URL, for [`NetMode::Proxy`].
    pub proxy: String,
    /// Unix socket paths the sandbox may reach.
    pub unix_sockets: Vec<String>,
    /// TLS-termination and body-inspection settings for CONNECT traffic.
    pub mitm: Option<MitmPolicy>,
}

/// TLS termination and content hooks for CONNECT traffic.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct MitmPolicy {
    /// Whether to intercept CONNECT traffic at all.
    pub enabled: bool,
    /// Whether to read request and response bodies.
    pub inspect_bodies: bool,
    /// The largest body to buffer, in bytes.
    pub max_body_bytes: i64,
    /// The hosts to intercept; empty means all of them.
    pub hosts: Vec<String>,
    /// The hosts never to intercept.
    pub exclude_hosts: Vec<String>,
}

impl NetPolicy {
    /// `NetPolicy.Validate`: the cross-backend invariants of a policy.
    ///
    /// The refusals are core's, in core's order and wording, because a
    /// configuration error is what an operator reads: the proxy URL is
    /// checked first, then the rules, then the socket paths, then the MITM
    /// settings. The proxy URL goes through [`url::parse`], so anything
    /// `url.Parse` refuses is refused here too -- see that module for the
    /// grammar and its deliberate limits.
    ///
    /// A refused policy is classified [`ErrorKind::Validation`], which is what
    /// core's `errdefs.Validationf` produces.
    pub fn validate(&self) -> Result<(), Error> {
        if !self.proxy.is_empty() {
            let parsed = url::parse(&self.proxy).map_err(|err| {
                validation(format!(
                    "net: invalid proxy URL {}: {err}",
                    text::quote(&self.proxy)
                ))
            })?;
            if parsed.scheme != "http" && parsed.scheme != "socks5" {
                return Err(validation(format!(
                    "net: proxy scheme {} must be http or socks5",
                    text::quote(&parsed.scheme)
                )));
            }
            if parsed.host.is_empty() {
                return Err(validation(format!(
                    "net: proxy URL {} has no host",
                    text::quote(&self.proxy)
                )));
            }
            if let Some(user) = &parsed.user
                && user.password.is_some()
                && user.username.is_empty()
            {
                return Err(validation(format!(
                    "net: proxy URL {} has a password but no username",
                    text::quote(&self.proxy)
                )));
            }
        }
        for (index, rule) in self.rules.iter().enumerate() {
            // `rule.action` is always one of the two, so core's check cannot
            // fail here (see the module docs).
            if rule.host.trim().is_empty() {
                return Err(validation(format!(
                    "net: net rule {index} has an empty host"
                )));
            }
            if rule.port < 0 || rule.port > 65535 {
                return Err(validation(format!(
                    "net: net rule {index} port {} out of range",
                    rule.port
                )));
            }
            if rule.reason.trim() != rule.reason {
                return Err(validation(format!(
                    "net: net rule {index} reason has leading or trailing whitespace"
                )));
            }
            if rule.reason.contains(['\r', '\n']) {
                return Err(validation(format!(
                    "net: net rule {index} reason must be a single line"
                )));
            }
        }
        for path in &self.unix_sockets {
            // `filepath.IsAbs`, which on the platforms this port targets means
            // a leading separator.
            if !path.starts_with('/') {
                return Err(validation(format!(
                    "net: unix socket path {} must be absolute",
                    text::quote(path)
                )));
            }
        }
        if let Some(mitm) = &self.mitm {
            if mitm.max_body_bytes < 0 {
                return Err(validation(
                    "net: mitm.max_body_bytes must be non-negative".to_owned(),
                ));
            }
            for (index, host) in mitm.hosts.iter().enumerate() {
                if host.trim().is_empty() {
                    return Err(validation(format!("net: mitm.hosts[{index}] is empty")));
                }
            }
            for (index, host) in mitm.exclude_hosts.iter().enumerate() {
                if host.trim().is_empty() {
                    return Err(validation(format!(
                        "net: mitm.exclude_hosts[{index}] is empty"
                    )));
                }
            }
        }
        Ok(())
    }
}

fn validation(message: String) -> Error {
    Error::new(ErrorKind::Validation, message)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_zero_value_is_the_strict_one() {
        let policy = NetPolicy::default();
        assert_eq!(policy.mode, NetMode::Default);
        assert!(policy.rules.is_empty());
        assert!(policy.allow_hosts.is_empty());
        assert!(policy.mitm.is_none());

        let rule = NetRule::default();
        assert_eq!(rule.action, NetAction::Deny);
        assert_eq!(rule.port, 0);
    }

    #[test]
    fn actions_print_the_way_core_prints_them() {
        assert_eq!(NetAction::Deny.as_str(), "deny");
        assert_eq!(NetAction::Allow.as_str(), "allow");
        assert_eq!(format!("{}", NetAction::Deny), "deny");
    }
}
