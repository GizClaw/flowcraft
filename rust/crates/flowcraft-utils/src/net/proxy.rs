//! The proxy's policy half: what one destination would be decided as.
//!
//! Core's `Proxy` is a listener, a mux and a tunnel. This is the question that
//! sits inside it, extracted so it can be asked without a socket:
//! [`Proxy::decide`] takes a `host:port`, applies the mode, the host rules and
//! the IP/CIDR rules, and answers with the audit record the serving proxy would
//! emit -- including the address the dial is pinned to.
//!
//! # The two passes, and why there are two
//!
//! A destination arrives as a name or as an address.
//!
//! - An **address** (`[::1]:8443`, `10.1.2.3:80`) is judged directly by
//!   [`Matcher::match_ip`]. If no rule covered it, the mode decides:
//!   allow-list refuses, proxy mode delegates to the upstream.
//! - A **name** is judged by [`Matcher::match_host`] first. If the rule set
//!   holds no IP/CIDR rules, that is the whole answer. If it does, the name is
//!   resolved *here*, by [`Proxy`], and every answer is judged; the dial then
//!   goes to an address the rules covered, never to a name a second resolution
//!   could point somewhere else. That pinning is what closes the rebinding
//!   hole, and [`Proxy::decide`] reports it as [`Audit::dial_host`].
//!
//! A refusal to resolve is a refusal to decide, so it fails closed: the record
//! is a deny carrying the resolver's refusal.
//!
//! # What is not here
//!
//! The listener, the HTTP and SOCKS5 wire, the upstream (an HTTP `CONNECT`
//! tunnel or a SOCKS5 dial) and TLS termination. Those are I/O and land with
//! the slice that serves traffic; this one is the decision they will ask for,
//! and it is what `rust/conformance/proxy-contract.json` pins.

use std::net::IpAddr;
use std::sync::Arc;

use flowcraft_contract::error::{Error, ErrorKind};

use super::addr::Addr;
use super::matcher::Matcher;
use super::policy::{MitmPolicy, NetAction, NetMode, NetPolicy, NetRule};

/// What a serving proxy is configured with: the policy, plus the settings only
/// the server reads.
///
/// `upstream` and `mitm` do not change any decision -- core's `decide` never
/// looks at them -- but they are part of the configuration, so a caller that
/// holds one of these holds everything the proxy would be started with.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ProxyConfig {
    /// Allow-list or proxy; the other two modes serve nothing.
    pub mode: NetMode,
    /// The legacy allow-list, compiled as allow rules after [`Self::rules`].
    pub allow_hosts: Vec<String>,
    /// The explicit rules.
    pub rules: Vec<NetRule>,
    /// The upstream proxy URL, for [`NetMode::Proxy`].
    pub upstream: String,
    /// TLS-termination settings, for the server.
    pub mitm: Option<MitmPolicy>,
}

impl ProxyConfig {
    /// The policy half of the configuration.
    pub fn policy(&self) -> NetPolicy {
        NetPolicy {
            mode: self.mode,
            allow_hosts: self.allow_hosts.clone(),
            rules: self.rules.clone(),
            proxy: self.upstream.clone(),
            unix_sockets: Vec::new(),
            mitm: self.mitm.clone(),
        }
    }
}

/// One decision, as the audit trail records it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Audit {
    /// The destination's host, as written (an IPv6 literal without brackets).
    pub host: String,
    /// The destination's port, from the request or the caller's default.
    pub port: i32,
    /// The verdict.
    pub action: NetAction,
    /// The mode the verdict came from.
    pub mode: NetMode,
    /// The decisive rule, described as core describes it (`"allow
    /// example.com:443"`), or empty when the mode decided.
    pub rule: String,
    /// The decisive deny rule's model-facing explanation, or empty.
    pub reason: String,
    /// The `host:port` a dial would use. Equal to the request unless the
    /// address pass pinned it.
    pub dial_host: String,
}

impl Audit {
    /// This record with a verdict written into it.
    fn settled(mut self, action: NetAction, rule: &str, reason: &str) -> Audit {
        self.action = action;
        self.rule = rule.to_owned();
        self.reason = reason.to_owned();
        self
    }
}

/// The answer to one [`Proxy::decide`] question.
#[derive(Debug)]
pub struct Verdict {
    /// The audit record. Always populated: a decision that could not be made
    /// is recorded as the deny it was served as.
    pub record: Audit,
    /// Why the decision could not be made, when it could not: the rules did not
    /// compile, or a resolution the IP rules needed failed. Core's errors here
    /// carry no classification, so they are [`ErrorKind::Internal`].
    pub error: Option<Error>,
}

/// Where a name's addresses come from.
///
/// The serving proxy uses [`SystemResolver`]; a test supplies its own, which is
/// how the address pass is exercised without a network.
pub trait Resolver: Send + Sync {
    /// Resolve one name. The `Err` text is the resolver's own, which is the
    /// only part of the failure core does not spell.
    fn lookup(&self, host: &str) -> Result<Vec<Addr>, String>;
}

/// The platform resolver, as `net.DefaultResolver` is.
#[derive(Debug, Clone, Copy, Default)]
pub struct SystemResolver;

impl Resolver for SystemResolver {
    fn lookup(&self, host: &str) -> Result<Vec<Addr>, String> {
        use std::net::ToSocketAddrs;

        let found = (host, 0u16)
            .to_socket_addrs()
            .map_err(|err| err.to_string())?;
        Ok(found
            .map(|addr| match addr.ip() {
                IpAddr::V4(v4) => Addr::V4(v4),
                IpAddr::V6(v6) => Addr::V6 {
                    addr: v6,
                    zone: None,
                },
            })
            .collect())
    }
}

/// The policy half of a proxy: a compiled matcher and the mode.
#[derive(Clone)]
pub struct Proxy {
    mode: NetMode,
    matcher: Option<Matcher>,
    resolver: Arc<dyn Resolver>,
}

impl std::fmt::Debug for Proxy {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Proxy")
            .field("mode", &self.mode)
            .field("matcher", &self.matcher)
            .finish_non_exhaustive()
    }
}

impl Proxy {
    /// Build a proxy over the platform resolver.
    pub fn new(config: &ProxyConfig) -> Proxy {
        Proxy::with_resolver(config, Arc::new(SystemResolver))
    }

    /// Build a proxy over a given resolver.
    ///
    /// A rule set that does not compile is kept as the error it is: core's
    /// `Start` accepts such a policy (only [`NetPolicy::validate`] runs there)
    /// and every decision then refuses, so the refusal belongs to the decision,
    /// not to the constructor.
    pub fn with_resolver(config: &ProxyConfig, resolver: Arc<dyn Resolver>) -> Proxy {
        Proxy {
            mode: config.mode,
            matcher: Matcher::new(&config.policy()).ok(),
            resolver,
        }
    }

    /// The mode this proxy serves in.
    pub fn mode(&self) -> NetMode {
        self.mode
    }

    /// `Proxy.Decide`: judge one destination without dialing it.
    ///
    /// `hostport` is `host:port`, `[v6]:port` or a bare host, in which case
    /// `default_port` fills the port in (443 for `CONNECT`, 80 for
    /// absolute-form HTTP, 0 for SOCKS5). The split is
    /// [`split_host_port`]'s, which is core's, including its two fallbacks.
    pub fn decide(&self, hostport: &str, default_port: i32) -> Verdict {
        let (host, port) = split_host_port(hostport, default_port);
        let record = Audit {
            host: host.clone(),
            port,
            action: NetAction::Deny,
            mode: self.mode,
            rule: String::new(),
            reason: String::new(),
            dial_host: hostport.to_owned(),
        };
        let matcher = match &self.matcher {
            Some(matcher) => matcher,
            None => {
                return Verdict {
                    record,
                    error: Some(internal("netproxy: policy matcher unavailable")),
                };
            }
        };

        if let Some(address) = Addr::parse(&host) {
            let decision = matcher.match_ip(&address.unmap(), port);
            let record = if decision.action == NetAction::Deny || decision.matched {
                record.settled(decision.action, &decision.rule, &decision.reason)
            } else {
                // An address no rule covered is the mode's to decide, exactly
                // as an uncovered name is.
                record.settled(self.default_action(), "", "")
            };
            return Verdict {
                record,
                error: None,
            };
        }

        let decision = matcher.match_host(&host, port);
        if decision.action == NetAction::Deny {
            return Verdict {
                record: record.settled(decision.action, &decision.rule, &decision.reason),
                error: None,
            };
        }
        if !matcher.has_ip_rules() {
            let record = if decision.matched {
                record.settled(NetAction::Allow, &decision.rule, &decision.reason)
            } else {
                record.settled(self.default_action(), "", "")
            };
            return Verdict {
                record,
                error: None,
            };
        }

        let addresses = match self.resolver.lookup(&host) {
            Ok(addresses) => addresses,
            Err(reason) => {
                return Verdict {
                    record,
                    error: Some(internal(format!(
                        "netproxy: resolve {host} for IP rules: {reason}"
                    ))),
                };
            }
        };
        let hostname = HostVerdict {
            matched: decision.matched,
            rule: decision.rule,
            reason: decision.reason,
        };
        let (action, rule, reason, dial_host) =
            self.decide_addresses(port, &record.dial_host, &addresses, &hostname, matcher);
        Verdict {
            record: record
                .settled(action, &rule, &reason)
                .with_dial_host(dial_host),
            error: None,
        }
    }

    /// The action the mode gives a destination no rule decided.
    fn default_action(&self) -> NetAction {
        if self.mode == NetMode::AllowList {
            NetAction::Deny
        } else {
            NetAction::Allow
        }
    }

    /// `decideAddrs`: apply the IP/CIDR rules to every address a name resolved
    /// to, and pick the address to dial.
    ///
    /// Two rules shape the result:
    ///
    /// - A deny match on **any** address denies the destination. With several
    ///   answers none of them may pass, because which one a dial would have
    ///   reached is not known.
    /// - The dial target is pinned to the first address an allow rule
    ///   actually covers, never to an uncovered address while another one is
    ///   allowed -- that is the reordering/rebinding hole. When no allow rule
    ///   covers any address, the hostname rule (or the mode default) is the
    ///   only authority, so the first answer is pinned instead; pinning
    ///   either way keeps the dial from resolving a second time.
    fn decide_addresses(
        &self,
        port: i32,
        dial_host: &str,
        addresses: &[Addr],
        hostname: &HostVerdict,
        matcher: &Matcher,
    ) -> (NetAction, String, String, String) {
        let mut pinned: Option<Addr> = None;
        let mut pin_rule = String::new();
        for address in addresses {
            let address = address.unmap();
            let decision = matcher.match_ip(&address, port);
            if !decision.matched {
                continue;
            }
            if decision.action == NetAction::Deny {
                return (
                    NetAction::Deny,
                    decision.rule,
                    decision.reason,
                    dial_host.to_owned(),
                );
            }
            if pinned.is_none() {
                pinned = Some(address);
                pin_rule = decision.rule;
            }
        }
        let address_allowed = pinned.is_some();
        let target = match pinned.or_else(|| addresses.first().map(Addr::unmap)) {
            Some(address) => join_host_port(&address.to_string(), port),
            None => dial_host.to_owned(),
        };
        if hostname.matched {
            (
                NetAction::Allow,
                hostname.rule.clone(),
                hostname.reason.clone(),
                target,
            )
        } else if address_allowed {
            (NetAction::Allow, pin_rule, String::new(), target)
        } else {
            (self.default_action(), String::new(), String::new(), target)
        }
    }
}

impl Audit {
    /// This record with another dial target.
    fn with_dial_host(mut self, dial_host: String) -> Audit {
        self.dial_host = dial_host;
        self
    }
}

/// The hostname half of a decision, carried into the address pass so the final
/// verdict can combine both.
struct HostVerdict {
    matched: bool,
    rule: String,
    reason: String,
}

fn internal(message: impl Into<String>) -> Error {
    Error::new(ErrorKind::Internal, message.into())
}

/// `splitHostPort` as core calls it, including the two fallbacks: brackets are
/// trimmed off a name the splitter refused, and a port that is not a number
/// becomes the default.
///
/// `net.SplitHostPort` itself is transcribed, because its refusals decide which
/// fallback runs: `[::1]` has no port (fallback), `::1:80` has too many colons
/// (fallback), `example.com:` splits with an empty port (no fallback, and the
/// port then defaults). See the contract's `split-*` rows.
pub fn split_host_port(hostport: &str, default_port: i32) -> (String, i32) {
    match split_host_port_strict(hostport) {
        Some((host, port)) => (host.to_owned(), port.parse().unwrap_or(default_port)),
        None => (hostport.trim_matches(['[', ']']).to_owned(), default_port),
    }
}

/// `net.SplitHostPort`: the split core falls back from, and the one the SOCKS5
/// client refuses on (`golang.org/x/net` calls it directly, so no fallback).
pub fn split_host_port_strict(hostport: &str) -> Option<(&str, &str)> {
    let bytes = hostport.as_bytes();
    let last_colon = hostport.rfind(':')?;
    if bytes.first() == Some(&b'[') {
        // The first `]` has to sit just before the last colon, and no second
        // bracket may appear anywhere.
        let end = bytes.iter().position(|byte| *byte == b']')?;
        if end + 1 != last_colon {
            return None;
        }
        if bytes[1..].contains(&b'[') || bytes[end + 1..].contains(&b']') {
            return None;
        }
        return Some((&hostport[1..end], &hostport[last_colon + 1..]));
    }
    let host = &hostport[..last_colon];
    if host.contains(':') || bytes.contains(&b'[') || bytes.contains(&b']') {
        return None;
    }
    Some((host, &hostport[last_colon + 1..]))
}

/// `net.JoinHostPort`: brackets around anything with a colon in it.
pub fn join_host_port(host: &str, port: i32) -> String {
    if host.contains(':') {
        format!("[{host}]:{port}")
    } else {
        format!("{host}:{port}")
    }
}

/// `denyReasonSuffix`: the deny rule's explanation as an HTTP body suffix.
pub fn deny_reason_suffix(reason: &str) -> String {
    if reason.is_empty() {
        String::new()
    } else {
        format!(": {reason}")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    struct Fixed(Vec<&'static str>);

    impl Resolver for Fixed {
        fn lookup(&self, _host: &str) -> Result<Vec<Addr>, String> {
            Ok(self.0.iter().filter_map(|raw| Addr::parse(raw)).collect())
        }
    }

    fn proxy(config: ProxyConfig, addresses: Vec<&'static str>) -> Proxy {
        Proxy::with_resolver(&config, Arc::new(Fixed(addresses)))
    }

    fn allow_list(rules: Vec<NetRule>) -> ProxyConfig {
        ProxyConfig {
            mode: NetMode::AllowList,
            rules,
            ..ProxyConfig::default()
        }
    }

    fn reported(verdict: Verdict) -> Error {
        match verdict.error {
            Some(error) => error,
            None => panic!("the decision should have reported an error"),
        }
    }

    fn rule(action: NetAction, host: &str) -> NetRule {
        NetRule {
            action,
            host: host.to_owned(),
            ..NetRule::default()
        }
    }

    /// core's TestDecideAddrsPinsTheAllowedAddress: an uncovered answer listed
    /// first must not become the dial target.
    #[test]
    fn the_dial_is_pinned_to_the_address_the_rules_covered() {
        let proxy = proxy(
            allow_list(vec![
                rule(NetAction::Allow, "allowed.example"),
                rule(NetAction::Allow, "93.184.216.34/32"),
            ]),
            vec!["10.0.0.5", "93.184.216.34"],
        );
        let verdict = proxy.decide("allowed.example:443", 443);
        assert_eq!(verdict.record.action, NetAction::Allow);
        assert_eq!(verdict.record.dial_host, "93.184.216.34:443");
        assert_eq!(verdict.record.rule, "allow allowed.example");
        assert!(verdict.error.is_none());
    }

    /// core's TestDecideAddrsDenyBeatsEveryAddress.
    #[test]
    fn a_denied_address_denies_the_whole_answer() {
        let proxy = proxy(
            allow_list(vec![
                rule(NetAction::Allow, "allowed.example"),
                NetRule {
                    action: NetAction::Deny,
                    host: "10.0.0.0/8".to_owned(),
                    reason: "metadata range".to_owned(),
                    ..NetRule::default()
                },
            ]),
            vec!["93.184.216.34", "10.0.0.5"],
        );
        let verdict = proxy.decide("allowed.example:443", 443);
        assert_eq!(verdict.record.action, NetAction::Deny);
        assert_eq!(verdict.record.reason, "metadata range");
        assert!(!verdict.record.rule.is_empty());
    }

    /// core's TestDecideAddrsUncoveredAnswerPinsFirstAddress: deny-only rules
    /// leave the hostname rule as the authority, and the first answer is what
    /// gets dialed.
    #[test]
    fn an_uncovered_answer_pins_its_first_address() {
        let proxy = proxy(
            allow_list(vec![
                rule(NetAction::Allow, "allowed.example"),
                rule(NetAction::Deny, "10.0.0.0/8"),
            ]),
            vec!["93.184.216.34", "2001:db8::1"],
        );
        let verdict = proxy.decide("allowed.example:443", 443);
        assert_eq!(verdict.record.action, NetAction::Allow);
        assert_eq!(verdict.record.dial_host, "93.184.216.34:443");
        assert_eq!(verdict.record.rule, "allow allowed.example");
    }

    /// core's TestDecideAddrsIPRuleAloneAllowsAndNames: with no hostname rule
    /// matched, the address rule is what decided, and the record says so.
    #[test]
    fn an_address_rule_alone_allows_and_is_named() {
        let proxy = proxy(
            allow_list(vec![rule(NetAction::Allow, "93.184.216.34:443")]),
            vec!["203.0.113.9", "93.184.216.34"],
        );
        let verdict = proxy.decide("cdn.example:443", 443);
        assert_eq!(verdict.record.action, NetAction::Allow);
        assert_eq!(verdict.record.rule, "allow 93.184.216.34:443");
        assert_eq!(verdict.record.dial_host, "93.184.216.34:443");
        // The same answer on a port no rule covers falls back to the mode
        // default, which allow-list mode reads as a denial.
        let verdict = proxy.decide("cdn.example:8443", 8443);
        assert_eq!(verdict.record.action, NetAction::Deny);
    }

    /// core's TestDecideAddrsPinsIPv6BracketedForm: a v4-mapped answer is
    /// unmapped before matching, so only the real v6 entry is covered, and the
    /// pinned target is bracketed.
    #[test]
    fn an_ipv6_pin_is_bracketed() {
        let proxy = proxy(
            allow_list(vec![rule(NetAction::Allow, "2001:db8::/32")]),
            vec!["::ffff:93.184.216.34", "2001:db8::1"],
        );
        let verdict = proxy.decide("v6.example:443", 443);
        assert_eq!(verdict.record.action, NetAction::Allow);
        assert_eq!(verdict.record.dial_host, "[2001:db8::1]:443");
    }

    /// core's TestDecideAddrsProxyModeDelegatesToUpstream.
    #[test]
    fn proxy_mode_delegates_an_uncovered_answer() {
        let proxy = proxy(
            ProxyConfig {
                mode: NetMode::Proxy,
                upstream: "http://127.0.0.1:3128".to_owned(),
                rules: vec![rule(NetAction::Deny, "10.0.0.0/8")],
                ..ProxyConfig::default()
            },
            vec!["93.184.216.34"],
        );
        let verdict = proxy.decide("any.example:443", 443);
        assert_eq!(verdict.record.action, NetAction::Allow);
        assert_eq!(verdict.record.dial_host, "93.184.216.34:443");
    }

    /// core's TestDecideCarriesTheHostnameVerdictPastResolution: a matched
    /// hostname rule stays the decisive rule through the address pass.
    #[test]
    fn a_hostname_rule_survives_resolution_as_the_decisive_rule() {
        let proxy = proxy(
            allow_list(vec![
                rule(NetAction::Allow, "localhost"),
                rule(NetAction::Deny, "10.0.0.0/8"),
            ]),
            vec!["127.0.0.1"],
        );
        let verdict = proxy.decide("localhost:443", 443);
        assert_eq!(verdict.record.action, NetAction::Allow);
        assert_eq!(verdict.record.rule, "allow localhost");
        assert_eq!(verdict.record.dial_host, "127.0.0.1:443");
    }

    #[test]
    fn a_resolution_failure_fails_closed() {
        struct Refusing;

        impl Resolver for Refusing {
            fn lookup(&self, host: &str) -> Result<Vec<Addr>, String> {
                Err(format!("lookup {host}: no such host"))
            }
        }

        let config = allow_list(vec![
            rule(NetAction::Allow, "example.com"),
            rule(NetAction::Deny, "10.0.0.0/8"),
        ]);
        let proxy = Proxy::with_resolver(&config, Arc::new(Refusing));
        let verdict = proxy.decide("example.com:443", 443);
        assert_eq!(verdict.record.action, NetAction::Deny);
        let error = reported(verdict);
        assert_eq!(
            error.to_string(),
            "netproxy: resolve example.com for IP rules: lookup example.com: no such host"
        );
    }

    #[test]
    fn a_rule_set_that_does_not_compile_denies_every_destination() {
        let proxy = Proxy::new(&allow_list(vec![rule(NetAction::Allow, "*")]));
        let verdict = proxy.decide("example.com:443", 443);
        assert_eq!(verdict.record.action, NetAction::Deny);
        assert_eq!(
            reported(verdict).to_string(),
            "netproxy: policy matcher unavailable"
        );
    }

    #[test]
    fn the_split_falls_back_the_way_core_does() {
        assert_eq!(
            split_host_port("example.com:443", 80),
            ("example.com".to_owned(), 443)
        );
        assert_eq!(
            split_host_port("example.com:bad", 80),
            ("example.com".to_owned(), 80)
        );
        assert_eq!(split_host_port("[::1]:", 80), ("::1".to_owned(), 80));
        // A refused split trims brackets, which is why `::1:80` comes back as
        // one host rather than as an address and a port.
        assert_eq!(split_host_port("::1:80", 80), ("::1:80".to_owned(), 80));
        assert_eq!(
            split_host_port("[[::1]]:80", 80),
            ("::1]]:80".to_owned(), 80)
        );
        assert_eq!(split_host_port("", 443), (String::new(), 443));
    }
}
