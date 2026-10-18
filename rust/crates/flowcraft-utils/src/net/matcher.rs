//! The matcher: which rule decides one destination, and what it says.
//!
//! Rules come in two halves and are applied in core's order:
//!
//! 1. every deny rule, in the order the policy lists them;
//! 2. the first allow rule that matches.
//!
//! A single deny therefore wins over any number of allows, wherever the deny
//! sits in the list. The legacy `allow_hosts` list is compiled as allow rules
//! *after* the explicit ones, so a deny there still beats an allow here -- the
//! property that let core introduce `rules` without changing what existing
//! configurations meant.
//!
//! A name and an address are asked separately, because they answer different
//! questions: [`Matcher::match_host`] never resolves DNS, and
//! [`Matcher::match_ip`] is what a caller asks *after* resolving, against the
//! addresses a connection actually lands on. A policy that lists only names
//! leaves every address unmatched, and the caller falls back on the mode.
//!
//! # Refusals are the constructor's, not the query's
//!
//! Hosts are normalized and patterns compiled once, in [`Matcher::new`], so a
//! malformed rule is a configuration error rather than something a connection
//! discovers. The messages are core's, including the `sandbox:` prefix it adds.

use flowcraft_contract::error::{Error, ErrorKind};
use flowcraft_contract::text;

use super::addr::Addr;
use super::host::{self, Pattern};
use super::policy::{NetAction, NetPolicy, NetRule};

/// What one query decided.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Decision {
    /// The verdict. [`NetAction::Allow`] when nothing matched -- core returns
    /// allow with `matched` false, and the caller applies the mode.
    pub action: NetAction,
    /// The decisive rule as core describes it (`"allow example.com:443"`), or
    /// empty when nothing matched.
    pub rule: String,
    /// The rule's model-facing explanation, if it carried one.
    pub reason: String,
    /// Whether a rule decided this query at all.
    pub matched: bool,
}

impl Decision {
    /// The answer for a destination no rule names.
    fn unmatched() -> Decision {
        Decision {
            action: NetAction::Allow,
            rule: String::new(),
            reason: String::new(),
            matched: false,
        }
    }
}

/// One rule, compiled.
#[derive(Debug, Clone, PartialEq, Eq)]
struct CompiledRule {
    action: NetAction,
    port: i32,
    desc: String,
    reason: String,
    host: Pattern,
}

/// The compiled form of a [`NetPolicy`].
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Matcher {
    rules: Vec<CompiledRule>,
    has_ip_rules: bool,
}

impl Matcher {
    /// Compile a policy into a matcher.
    ///
    /// The policy is expected to have passed `NetPolicy.Validate`; this
    /// constructor re-checks the rule forms it needs (the port suffix and the
    /// host pattern) and refuses them with core's sentences and classification.
    pub fn new(policy: &NetPolicy) -> Result<Matcher, Error> {
        let mut matcher = Matcher::default();
        for (index, rule) in policy.rules.iter().enumerate() {
            let compiled = compile_rule(rule).map_err(|message| {
                Error::new(
                    ErrorKind::Validation,
                    format!("sandbox: net rule {index}: {message}"),
                )
            })?;
            if compiled.host.is_ip() {
                matcher.has_ip_rules = true;
            }
            matcher.rules.push(compiled);
        }
        for entry in &policy.allow_hosts {
            let rule = NetRule {
                action: NetAction::Allow,
                host: entry.clone(),
                port: 0,
                reason: String::new(),
            };
            let compiled = compile_rule(&rule).map_err(|message| {
                Error::new(
                    ErrorKind::Validation,
                    format!(
                        "sandbox: allow_hosts entry {}: {message}",
                        text::quote(entry)
                    ),
                )
            })?;
            if compiled.host.is_ip() {
                matcher.has_ip_rules = true;
            }
            matcher.rules.push(compiled);
        }
        Ok(matcher)
    }

    /// `Matcher.HasIPRules`: does any rule need resolved addresses?
    pub fn has_ip_rules(&self) -> bool {
        self.has_ip_rules
    }

    /// Judge a hostname and port. Never resolves DNS; ask
    /// [`Matcher::match_ip`] for the addresses a name resolved to.
    ///
    /// Core normalizes the name here and again inside the pattern; the pattern
    /// does it once, which is the same answer because normalization is
    /// idempotent.
    pub fn match_host(&self, host: &str, port: i32) -> Decision {
        let denies = self.rules.iter().filter(|rule| {
            rule.action == NetAction::Deny
                && !rule.host.is_ip()
                && rule.host.matches_host(host)
                && port_matches(rule.port, port)
        });
        if let Some(rule) = denies.into_iter().next() {
            return Decision {
                action: NetAction::Deny,
                rule: rule.desc.clone(),
                reason: rule.reason.clone(),
                matched: true,
            };
        }
        let allows = self.rules.iter().find(|rule| {
            !rule.host.is_ip() && rule.host.matches_host(host) && port_matches(rule.port, port)
        });
        match allows {
            Some(rule) => Decision {
                action: NetAction::Allow,
                rule: rule.desc.clone(),
                reason: rule.reason.clone(),
                matched: true,
            },
            None => Decision::unmatched(),
        }
    }

    /// Judge one resolved address and port, the same way.
    pub fn match_ip(&self, ip: &Addr, port: i32) -> Decision {
        let decides =
            |rule: &CompiledRule| rule.host.matches_ip(ip) && port_matches(rule.port, port);
        let denies = self
            .rules
            .iter()
            .filter(|rule| rule.action == NetAction::Deny && decides(rule));
        if let Some(rule) = denies.into_iter().next() {
            return Decision {
                action: NetAction::Deny,
                rule: rule.desc.clone(),
                reason: rule.reason.clone(),
                matched: true,
            };
        }
        match self.rules.iter().find(|rule| decides(rule)) {
            Some(rule) => Decision {
                action: NetAction::Allow,
                rule: rule.desc.clone(),
                reason: rule.reason.clone(),
                matched: true,
            },
            None => Decision::unmatched(),
        }
    }
}

/// Compile one rule, or hand back core's message for what was wrong with it.
fn compile_rule(rule: &NetRule) -> Result<CompiledRule, String> {
    let (host, port) = split_rule_host_port(&rule.host, rule.port)?;
    let pattern = host::compile(&host).map_err(|err| err.to_string())?;
    let mut desc = format!("{} {host}", rule.action.as_str());
    if port != 0 {
        desc.push_str(&format!(":{port}"));
    }
    Ok(CompiledRule {
        action: rule.action,
        port,
        desc,
        reason: rule.reason.clone(),
        host: pattern,
    })
}

/// The rule host with its optional `:port` suffix taken off.
///
/// This is the shape half of `net.SplitHostPort`: a bracketed IPv6 literal may
/// carry a port (`[::1]:8443`), an unbracketed host may carry exactly one colon,
/// and a bare IPv6 literal carries none, so `::1` falls through as an address
/// and `a:1:2` falls through as a name. A port that parses is range-checked;
/// one that does not is a refusal rather than a hostname, because a rule that
/// says `example.com:not-a-port` almost certainly did not mean the literal
/// name.
fn split_rule_host_port(host: &str, port: i32) -> Result<(String, i32), String> {
    let invalid = || format!("invalid port suffix {}", text::quote(host));
    let Some(index) = host.rfind(':') else {
        return Ok((host.to_owned(), port));
    };
    let (head, suffix) = (&host[..index], &host[index + 1..]);
    let head = match head.strip_prefix('[') {
        Some(inner) => match inner.strip_suffix(']') {
            Some(inner) => inner,
            None => return Ok((host.to_owned(), port)),
        },
        None => {
            if head.contains(['[', ']', ':']) {
                return Ok((host.to_owned(), port));
            }
            head
        }
    };
    let parsed = suffix.parse::<i64>().map_err(|_| invalid())?;
    // An empty head (`:443`) is not refused here: it reaches the pattern
    // compiler, which refuses it as an empty host, exactly as core does.
    if !(0..=65535).contains(&parsed) {
        return Err(invalid());
    }
    let parsed = i32::try_from(parsed).map_err(|_| invalid())?;
    Ok((head.to_owned(), parsed))
}

/// `portMatches`: a rule port of 0 means every port.
fn port_matches(rule_port: i32, port: i32) -> bool {
    rule_port == 0 || rule_port == port
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::net::{NetMode, NetRule};

    fn rule(action: NetAction, host: &str, port: i32, reason: &str) -> NetRule {
        NetRule {
            action,
            host: host.to_owned(),
            port,
            reason: reason.to_owned(),
        }
    }

    /// The refusal, which every caller of this in the tests expects.
    fn refused(policy: &NetPolicy) -> Error {
        match Matcher::new(policy) {
            Ok(_) => panic!("the policy should have been refused"),
            Err(err) => err,
        }
    }

    fn addr(text: &str) -> Addr {
        match Addr::parse(text) {
            Some(addr) => addr,
            None => panic!("{text:?} parses"),
        }
    }

    #[test]
    fn a_deny_anywhere_in_the_list_beats_an_allow_before_it() {
        let policy = NetPolicy {
            mode: NetMode::AllowList,
            rules: vec![
                rule(NetAction::Allow, "example.com", 0, ""),
                rule(
                    NetAction::Deny,
                    "*.example.com",
                    0,
                    "subdomains are not allowed",
                ),
                rule(NetAction::Allow, "api.example.com", 0, ""),
            ],
            ..NetPolicy::default()
        };
        let matcher = match Matcher::new(&policy) {
            Ok(matcher) => matcher,
            Err(err) => panic!("{err}"),
        };
        let decision = matcher.match_host("api.example.com", 443);
        assert_eq!(decision.action, NetAction::Deny);
        assert_eq!(decision.rule, "deny *.example.com");
        assert_eq!(decision.reason, "subdomains are not allowed");
        assert!(decision.matched);
        // The bare domain is not a subdomain, so the first allow decides it.
        assert_eq!(
            matcher.match_host("example.com", 443).action,
            NetAction::Allow
        );
    }

    #[test]
    fn a_rule_without_a_port_matches_every_port() {
        let policy = NetPolicy {
            rules: vec![
                rule(NetAction::Allow, "example.com", 22, ""),
                rule(NetAction::Deny, "example.com", 0, "ssh egress is blocked"),
            ],
            ..NetPolicy::default()
        };
        let matcher = match Matcher::new(&policy) {
            Ok(matcher) => matcher,
            Err(err) => panic!("{err}"),
        };
        assert_eq!(
            matcher.match_host("example.com", 22).rule,
            "deny example.com"
        );
        assert_eq!(
            matcher.match_host("example.com", 443).reason,
            "ssh egress is blocked"
        );
    }

    #[test]
    fn the_allow_list_is_compiled_after_the_rules() {
        let policy = NetPolicy {
            allow_hosts: vec!["example.com".to_owned()],
            rules: vec![rule(NetAction::Deny, "example.com", 25, "smtp")],
            ..NetPolicy::default()
        };
        let matcher = match Matcher::new(&policy) {
            Ok(matcher) => matcher,
            Err(err) => panic!("{err}"),
        };
        assert_eq!(
            matcher.match_host("example.com", 25).action,
            NetAction::Deny
        );
        assert_eq!(
            matcher.match_host("example.com", 443).rule,
            "allow example.com"
        );
    }

    #[test]
    fn an_unmatched_query_is_allowed_and_says_so() {
        let matcher = Matcher::default();
        let decision = matcher.match_host("example.com", 443);
        assert_eq!(decision.action, NetAction::Allow);
        assert!(!decision.matched);
        assert!(decision.rule.is_empty());
        assert!(!matcher.match_ip(&addr("10.1.2.3"), 443).matched);
        assert!(!matcher.has_ip_rules());
    }

    #[test]
    fn names_and_addresses_are_asked_separately() {
        let policy = NetPolicy {
            rules: vec![
                rule(NetAction::Deny, "10.1.2.3", 0, "one bad host"),
                rule(NetAction::Allow, "10.0.0.0/8", 0, ""),
                rule(NetAction::Allow, "example.com", 0, ""),
            ],
            ..NetPolicy::default()
        };
        let matcher = match Matcher::new(&policy) {
            Ok(matcher) => matcher,
            Err(err) => panic!("{err}"),
        };
        assert!(matcher.has_ip_rules());
        // The name "10.1.2.3" is not the address 10.1.2.3: no name rule
        // matches it.
        assert!(!matcher.match_host("10.1.2.3", 443).matched);
        assert_eq!(
            matcher.match_ip(&addr("10.1.2.3"), 443).action,
            NetAction::Deny
        );
        assert_eq!(
            matcher.match_ip(&addr("10.2.3.4"), 443).action,
            NetAction::Allow
        );
        assert!(!matcher.match_ip(&addr("11.0.0.0"), 443).matched);
        // The mapped form is judged by the address it wraps.
        assert_eq!(
            matcher.match_ip(&addr("::ffff:10.1.2.3"), 443).action,
            NetAction::Deny
        );
    }

    #[test]
    fn a_cidr_rule_is_a_prefix_not_a_name() {
        let policy = NetPolicy {
            rules: vec![rule(NetAction::Allow, "10.0.0.0/8:53", 0, "")],
            ..NetPolicy::default()
        };
        let matcher = match Matcher::new(&policy) {
            Ok(matcher) => matcher,
            Err(err) => panic!("{err}"),
        };
        assert_eq!(
            matcher.match_ip(&addr("10.1.2.3"), 53).rule,
            "allow 10.0.0.0/8:53"
        );
        assert!(!matcher.match_ip(&addr("10.1.2.3"), 5353).matched);
        assert!(!matcher.match_host("10.0.0.0", 53).matched);
    }

    #[test]
    fn a_bracketed_literal_is_an_address_only_when_it_carries_a_port() {
        // `[::1]:8443` is the bracketed spelling of an address *with* a port,
        // so `SplitHostPort` takes it apart and the rule is ::1:8443. `[::1]`
        // on its own has no port to split, so the brackets survive into a
        // hostname that matches the literal text and nothing else.
        let policy = NetPolicy {
            rules: vec![rule(NetAction::Allow, "[::2]", 0, "")],
            allow_hosts: vec!["[::1]:8443".to_owned()],
            ..NetPolicy::default()
        };
        let matcher = match Matcher::new(&policy) {
            Ok(matcher) => matcher,
            Err(err) => panic!("{err}"),
        };
        assert_eq!(matcher.match_ip(&addr("::1"), 8443).rule, "allow ::1:8443");
        assert!(!matcher.match_ip(&addr("::1"), 443).matched);
        assert_eq!(matcher.match_host("[::2]", 443).rule, "allow [::2]");
        assert!(!matcher.match_host("[::1]", 8443).matched);
        assert!(!matcher.match_host("::1", 8443).matched);
    }

    #[test]
    fn malformed_rules_are_refused_with_cores_sentences() {
        let policy = NetPolicy {
            rules: vec![NetRule::default()],
            ..NetPolicy::default()
        };
        assert_eq!(
            refused(&policy).to_string(),
            "sandbox: net rule 0: empty host"
        );

        let policy = NetPolicy {
            allow_hosts: vec!["*bad.example".to_owned()],
            ..NetPolicy::default()
        };
        assert_eq!(
            refused(&policy).to_string(),
            "sandbox: allow_hosts entry \"*bad.example\": host \"*bad.example\": \
             \"*\" is only allowed as a leading \"*.\" wildcard"
        );

        for host in [
            "example.com:not-a-port",
            "example.com:",
            "example.com:99999",
        ] {
            let policy = NetPolicy {
                allow_hosts: vec![host.to_owned()],
                ..NetPolicy::default()
            };
            let err = refused(&policy);
            assert_eq!(err.kind(), ErrorKind::Validation);
            assert!(
                err.to_string()
                    .ends_with(&format!("invalid port suffix \"{host}\"")),
                "{err}"
            );
        }
    }

    #[test]
    fn a_port_suffix_is_taken_off_only_when_it_is_one() {
        // Bracketed, unbracketed and bare: one colon means a port, two mean a
        // name, and nothing means "use the field".
        // One colon means a port, two mean a name, and nothing means "use the
        // field": the shape decides, and a shape that does not parse keeps the
        // whole string, which the pattern compiler then reads as a name.
        let cases = [
            ("[::1]:8443", 0, ("::1", 8443)),
            ("example.com:443", 0, ("example.com", 443)),
            ("example.com:0", 0, ("example.com", 0)),
            ("example.com", 22, ("example.com", 22)),
            ("+5", 0, ("+5", 0)),
            ("bare.example::1", 0, ("bare.example::1", 0)),
            ("colon.example:443:80", 0, ("colon.example:443:80", 0)),
            ("[::1]", 0, ("[::1]", 0)),
            ("::1", 0, ("::1", 0)),
        ];
        for (host, port, (expected_host, expected_port)) in cases {
            assert_eq!(
                split_rule_host_port(host, port),
                Ok((expected_host.to_owned(), expected_port)),
                "{host:?}"
            );
        }
        // A leading "+" parses, as `strconv.Atoi` allows it.
        assert_eq!(
            split_rule_host_port("example.com:+8", 0),
            Ok(("example.com".to_owned(), 8))
        );
    }
}
