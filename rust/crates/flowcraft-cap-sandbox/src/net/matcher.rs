//! The policy kernel of network enforcement: one decision per destination.
//!
//! The proxy asks this module; nothing else decides. Four rules carry the whole
//! security model, and each is a bug if it is reversed:
//!
//! 1. **Deny wins across the entire rule set.** A single deny match beats any
//!    allow, no matter where the rules appear.
//! 2. **`allow_hosts` are trailing allow rules**, so an explicit deny always
//!    outranks the legacy list.
//! 3. **A failed resolution is a denial** when address rules exist: we cannot
//!    prove the destination is not denied, so it does not pass.
//! 4. **The dialled address is pinned** to one the rules actually validated, so
//!    a second resolution (a rebinding answer) cannot steer the connection
//!    somewhere the checks never saw.
//!
//! The module never resolves anything itself: the caller passes the answers it
//! got. That keeps every rule above testable offline.

use std::net::IpAddr;

use flowcraft_error::{Error, Result};

use super::pattern::Pattern;
use crate::contract::{NetAction, NetMode, NetPolicy, NetRule};

/// One compiled rule.
#[derive(Debug, Clone)]
struct Rule {
    action: NetAction,
    pattern: Pattern,
    /// `None` matches any port.
    port: Option<u16>,
    /// The model-facing explanation attached to a deny.
    reason: Option<String>,
    /// How the rule was written, for the audit record.
    description: String,
}

impl Rule {
    fn compile(rule: &NetRule) -> Result<Self> {
        let (host, embedded_port) = split_port(&rule.host)?;
        let declared = Some(rule.port).filter(|port| *port != 0);
        let port = match (declared, embedded_port) {
            (Some(a), Some(b)) if a != b => {
                return Err(Error::validation(format!(
                    "sandbox: net rule {:?} names two different ports ({a} and {b})",
                    rule.host
                )));
            }
            (Some(port), _) | (None, Some(port)) => Some(port),
            (None, None) => None,
        };
        let pattern = Pattern::parse(host)?;
        Ok(Self {
            action: rule.action,
            description: match port {
                Some(port) => format!("{}:{port}", pattern.describe()),
                None => pattern.describe(),
            },
            pattern,
            port,
            reason: rule.reason.clone(),
        })
    }

    fn catches_host(&self, host: &str, port: u16) -> bool {
        !self.pattern.is_ip()
            && self.port.is_none_or(|wanted| wanted == port)
            && self.pattern.matches_host(host)
    }

    fn catches_ip(&self, ip: IpAddr, port: u16) -> bool {
        self.pattern.is_ip()
            && self.port.is_none_or(|wanted| wanted == port)
            && self.pattern.matches_ip(ip)
    }
}

/// Split a `:port` suffix off a rule host.
///
/// A bracketed IPv6 literal keeps its brackets' contents; a bare IPv6 literal
/// with no port is ambiguous, so it is parsed as an address first.
fn split_port(raw: &str) -> Result<(&str, Option<u16>)> {
    if raw.starts_with('[') {
        // `[::1]:8443` or `[::1]`
        return match raw.find(']') {
            Some(end) => {
                let host = &raw[..=end];
                let rest = &raw[end + 1..];
                match rest.strip_prefix(':') {
                    Some(port) => Ok((host, Some(parse_port(port, raw)?))),
                    None if rest.is_empty() => Ok((host, None)),
                    None => Err(Error::validation(format!(
                        "sandbox: net rule host {raw:?} has trailing junk after ']'"
                    ))),
                }
            }
            None => Err(Error::validation(format!(
                "sandbox: net rule host {raw:?} has an unclosed '['"
            ))),
        };
    }
    if raw.parse::<IpAddr>().is_ok() {
        return Ok((raw, None));
    }
    match raw.rsplit_once(':') {
        Some((host, port)) if !host.is_empty() && port.chars().all(|ch| ch.is_ascii_digit()) => {
            Ok((host, Some(parse_port(port, raw)?)))
        }
        _ => Ok((raw, None)),
    }
}

fn parse_port(port: &str, raw: &str) -> Result<u16> {
    port.parse::<u16>()
        .map_err(|_| Error::validation(format!("sandbox: net rule host {raw:?} has a bad port")))
}

/// One destination's verdict.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct Verdict {
    /// Whether the connection may proceed.
    pub(crate) allow: bool,
    /// The deciding rule, when one matched.
    pub(crate) rule: Option<String>,
    /// The deny rule's model-facing explanation.
    pub(crate) reason: Option<String>,
    /// The address to dial, when the decision pinned one.
    pub(crate) pin: Option<IpAddr>,
}

impl Verdict {
    fn mode_default(mode: NetMode) -> Self {
        Self {
            allow: mode != NetMode::AllowList,
            rule: None,
            reason: None,
            pin: None,
        }
    }

    fn allow(rule: String, pin: Option<IpAddr>) -> Self {
        Self {
            allow: true,
            rule: Some(rule),
            reason: None,
            pin,
        }
    }

    fn deny(rule: Option<String>, reason: Option<String>, pin: Option<IpAddr>) -> Self {
        Self {
            allow: false,
            rule,
            reason,
            pin,
        }
    }
}

/// The compiled policy.
#[derive(Debug)]
pub(crate) struct Matcher {
    rules: Vec<Rule>,
    mode: NetMode,
    has_ip_rules: bool,
}

impl Matcher {
    /// Compile a policy. `NetPolicy::validate` runs first, so a malformed policy
    /// is rejected before anything is compiled.
    pub(crate) fn new(policy: &NetPolicy) -> Result<Self> {
        policy.validate()?;
        let mut rules = Vec::with_capacity(policy.rules.len() + policy.allow_hosts.len());
        for rule in &policy.rules {
            rules.push(Rule::compile(rule)?);
        }
        // The legacy allow-list is appended so every explicit deny outranks it.
        for host in &policy.allow_hosts {
            rules.push(Rule::compile(&NetRule {
                action: NetAction::Allow,
                host: host.clone(),
                port: 0,
                reason: None,
            })?);
        }
        let has_ip_rules = rules.iter().any(|rule| rule.pattern.is_ip());
        Ok(Self {
            rules,
            mode: policy.mode,
            has_ip_rules,
        })
    }

    /// Whether any rule needs a resolved address.
    pub(crate) fn has_ip_rules(&self) -> bool {
        self.has_ip_rules
    }

    /// Whether there is nothing to enforce.
    pub(crate) fn is_empty(&self) -> bool {
        self.rules.is_empty()
    }

    /// Decide one destination.
    ///
    /// `addrs` are the answers a name resolved to; pass `None` when no
    /// resolution was attempted. When address rules exist and `addrs` is `None`,
    /// the verdict is a denial — see rule 3 in the module docs.
    pub(crate) fn decide(&self, host: &str, port: u16, addrs: Option<&[IpAddr]>) -> Verdict {
        // An address literal is only answered by address rules.
        if let Ok(ip) = host.parse::<IpAddr>() {
            let verdict = self.decide_ip(ip, port);
            if !verdict.allow || verdict.rule.is_some() {
                return verdict;
            }
            return Verdict::mode_default(self.mode);
        }

        let host_verdict = self.decide_host(host, port);
        if !host_verdict.allow {
            return host_verdict;
        }
        if !self.has_ip_rules {
            return if host_verdict.rule.is_some() {
                host_verdict
            } else {
                Verdict::mode_default(self.mode)
            };
        }

        let Some(addrs) = addrs else {
            return Verdict::deny(
                None,
                Some("could not resolve the host to check address rules".to_owned()),
                None,
            );
        };

        self.decide_addrs(port, addrs, &host_verdict)
    }

    /// The hostname pass: every deny, then the first allow.
    fn decide_host(&self, host: &str, port: u16) -> Verdict {
        for rule in &self.rules {
            if rule.action == NetAction::Deny && rule.catches_host(host, port) {
                return Verdict::deny(Some(rule.description.clone()), rule.reason.clone(), None);
            }
        }
        for rule in &self.rules {
            if rule.action == NetAction::Allow && rule.catches_host(host, port) {
                return Verdict::allow(rule.description.clone(), None);
            }
        }
        // Nothing matched: the mode decides, and the caller must not read this
        // as "matched".
        Verdict::mode_default(self.mode)
    }

    /// The address pass: every deny, then pin one allowed address.
    fn decide_ip(&self, ip: IpAddr, port: u16) -> Verdict {
        for rule in &self.rules {
            if rule.action == NetAction::Deny && rule.catches_ip(ip, port) {
                return Verdict::deny(Some(rule.description.clone()), rule.reason.clone(), None);
            }
        }
        for rule in &self.rules {
            if rule.action == NetAction::Allow && rule.catches_ip(ip, port) {
                return Verdict::allow(rule.description.clone(), Some(ip));
            }
        }
        Verdict::mode_default(self.mode)
    }

    /// Combine the hostname and address passes and pin the dial target.
    fn decide_addrs(&self, port: u16, addrs: &[IpAddr], host: &Verdict) -> Verdict {
        let mut pinned = None;
        let mut pin_rule = None;
        for ip in addrs {
            let verdict = self.decide_ip(*ip, port);
            if !verdict.allow && verdict.rule.is_some() {
                // A *rule* denied this answer, so the destination is denied: with
                // several answers we cannot know which one a dial would reach. A
                // mode default that simply did not match is not a deny here — it
                // is the pinning scan seeing an uncovered answer.
                return verdict;
            }
            if pinned.is_none() && verdict.allow && verdict.rule.is_some() {
                pinned = Some(*ip);
                pin_rule = verdict.rule;
            }
        }
        let ip_allowed = pinned.is_some();
        if pinned.is_none() {
            // No allow rule covered any answer: the hostname rule (or the mode)
            // is the only authority, but the dial is still pinned so a second
            // resolution cannot steer it.
            pinned = addrs.first().copied();
        }
        match (host.rule.is_some(), ip_allowed) {
            (true, _) => Verdict::allow(host.rule.clone().unwrap_or_default(), pinned),
            (false, true) => Verdict::allow(pin_rule.unwrap_or_default(), pinned),
            (false, false) => {
                let default = Verdict::mode_default(self.mode);
                if default.allow {
                    Verdict::allow(String::new(), pinned)
                } else {
                    Verdict::deny(None, None, pinned)
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::contract::NetPolicy;

    fn policy(mode: NetMode, rules: Vec<NetRule>, allow_hosts: Vec<&str>) -> NetPolicy {
        NetPolicy {
            mode,
            rules,
            allow_hosts: allow_hosts.into_iter().map(str::to_owned).collect(),
            ..NetPolicy::default()
        }
    }

    fn allow(host: &str) -> NetRule {
        NetRule {
            action: NetAction::Allow,
            host: host.to_owned(),
            port: 0,
            reason: None,
        }
    }

    fn deny(host: &str, reason: &str) -> NetRule {
        NetRule {
            action: NetAction::Deny,
            host: host.to_owned(),
            port: 0,
            reason: Some(reason.to_owned()),
        }
    }

    fn matcher(policy: &NetPolicy) -> Matcher {
        Matcher::new(policy).expect("matcher")
    }

    #[test]
    fn an_allow_list_denies_everything_it_does_not_name() {
        let matcher = matcher(&policy(
            NetMode::AllowList,
            vec![allow("api.example.com")],
            vec![],
        ));
        assert!(matcher.decide("api.example.com", 443, None).allow);
        let denied = matcher.decide("other.example.com", 443, None);
        assert!(!denied.allow);
        assert!(denied.rule.is_none(), "the mode decided, not a rule");
    }

    #[test]
    fn deny_wins_over_an_allow_wherever_it_appears() {
        for rules in [
            vec![deny("evil.example.com", "blocked"), allow("example.com")],
            vec![allow("example.com"), deny("evil.example.com", "blocked")],
        ] {
            let matcher = matcher(&policy(NetMode::AllowList, rules, vec![]));
            let verdict = matcher.decide("evil.example.com", 443, None);
            assert!(!verdict.allow, "a deny must beat a broader allow");
            assert_eq!(verdict.reason.as_deref(), Some("blocked"));
        }
    }

    #[test]
    fn an_explicit_deny_outranks_the_legacy_allow_list() {
        let matcher = matcher(&policy(
            NetMode::Proxy,
            vec![deny("api.example.com", "no")],
            vec!["example.com"],
        ));
        assert!(!matcher.decide("api.example.com", 443, None).allow);
        assert!(matcher.decide("web.example.com", 443, None).allow);
    }

    #[test]
    fn a_port_suffix_restricts_one_destination() {
        let matcher = matcher(&policy(
            NetMode::AllowList,
            vec![allow("example.com:443")],
            vec![],
        ));
        assert!(matcher.decide("example.com", 443, None).allow);
        assert!(!matcher.decide("example.com", 80, None).allow);
    }

    #[test]
    fn a_rule_port_and_a_host_port_must_agree() {
        let mut rule = allow("example.com:443");
        rule.port = 8443;
        let error =
            Matcher::new(&policy(NetMode::AllowList, vec![rule], vec![])).expect_err("conflict");
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::Validation);
    }

    #[test]
    fn proxy_mode_allows_by_default_and_still_honours_denies() {
        let matcher = matcher(&policy(
            NetMode::Proxy,
            vec![deny("api.example.com", "no")],
            vec![],
        ));
        assert!(matcher.decide("anything.example", 443, None).allow);
        assert!(!matcher.decide("api.example.com", 443, None).allow);
    }

    #[test]
    fn an_address_literal_is_answered_by_address_rules_only() {
        let matcher = matcher(&policy(
            NetMode::AllowList,
            vec![allow("10.0.0.0/8")],
            vec![],
        ));
        assert!(matcher.decide("10.1.2.3", 5432, None).allow);
        assert!(!matcher.decide("11.1.2.3", 5432, None).allow);
    }

    #[test]
    fn a_denied_address_answer_denies_the_whole_destination() {
        let matcher = matcher(&policy(
            NetMode::AllowList,
            vec![allow("example.com"), deny("10.0.0.0/8", "internal")],
            vec![],
        ));
        let addrs = [
            "93.184.216.34".parse().unwrap(),
            "10.1.2.3".parse().unwrap(),
        ];
        let verdict = matcher.decide("example.com", 443, Some(&addrs));
        assert!(!verdict.allow, "any denied answer denies the destination");
    }

    #[test]
    fn the_dial_target_is_pinned_to_a_validated_address() {
        let matcher = matcher(&policy(
            NetMode::AllowList,
            vec![allow("example.com"), allow("10.0.0.0/8")],
            vec![],
        ));
        // The first answer is not covered by an address rule; the second is.
        let addrs = ["203.0.113.9".parse().unwrap(), "10.1.2.3".parse().unwrap()];
        let verdict = matcher.decide("example.com", 443, Some(&addrs));
        assert!(verdict.allow);
        assert_eq!(
            verdict.pin,
            Some("10.1.2.3".parse().unwrap()),
            "pin the address a rule actually covered, not the first answer"
        );
    }

    #[test]
    fn a_hostname_only_rule_still_pins_an_answer() {
        let matcher = matcher(&policy(
            NetMode::AllowList,
            vec![allow("example.com")],
            vec![],
        ));
        assert!(!matcher.has_ip_rules());
        let addrs = ["203.0.113.9".parse().unwrap()];
        let verdict = matcher.decide("example.com", 443, Some(&addrs));
        // Without address rules the name is authoritative; nothing is pinned by
        // a rule, and none is needed.
        assert!(verdict.allow);
        assert_eq!(verdict.pin, None);
    }

    #[test]
    fn a_failed_resolution_is_a_denial_when_address_rules_exist() {
        let matcher = matcher(&policy(
            NetMode::Proxy,
            vec![allow("example.com"), deny("10.0.0.0/8", "internal")],
            vec![],
        ));
        assert!(matcher.has_ip_rules());
        let verdict = matcher.decide("example.com", 443, None);
        assert!(
            !verdict.allow,
            "with address rules present, an unresolvable name is refused"
        );
        assert!(
            verdict
                .reason
                .as_deref()
                .unwrap_or_default()
                .contains("resolve")
        );
    }

    #[test]
    fn a_name_outside_the_rules_follows_the_mode_even_with_address_rules() {
        let matcher = matcher(&policy(
            NetMode::Proxy,
            vec![allow("example.com"), deny("10.0.0.0/8", "internal")],
            vec![],
        ));
        let addrs = ["93.184.216.34".parse().unwrap()];
        let verdict = matcher.decide("other.example", 443, Some(&addrs));
        assert!(verdict.allow, "proxy mode delegates the rest upstream");
        assert_eq!(verdict.pin, Some("93.184.216.34".parse().unwrap()));
    }

    #[test]
    fn a_malformed_policy_never_compiles() {
        let mut policy = policy(NetMode::AllowList, vec![allow("example.com")], vec![]);
        policy.proxy = Some("http://127.0.0.1:1".to_owned());
        assert!(
            Matcher::new(&policy).is_err(),
            "a proxy URL outside proxy mode is a config bug"
        );
    }

    #[test]
    fn an_empty_policy_follows_the_mode() {
        let allow_list = matcher(&policy(NetMode::AllowList, vec![], vec![]));
        assert!(!allow_list.decide("example.com", 443, None).allow);
        assert!(allow_list.is_empty());

        let proxy = matcher(&policy(NetMode::Proxy, vec![], vec![]));
        assert!(proxy.decide("example.com", 443, None).allow);
    }
}
