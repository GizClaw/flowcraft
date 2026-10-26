//! Host patterns: what one net rule can name.
//!
//! The grammar is the one operators already write in a deployment document:
//!
//! | Entry | Meaning |
//! | --- | --- |
//! | `example.com` | the domain **and** every subdomain |
//! | `*.example.com` | subdomains only, never the bare domain |
//! | `example.com:443` | the same, restricted to one destination port |
//! | `1.2.3.4` | one address |
//! | `10.0.0.0/8` | a CIDR block |
//!
//! Parsing is strict and matching is case-insensitive: a rule that cannot be
//! understood is a `Validation` error at build time rather than a pattern that
//! matches nothing (which would look like a working deny) or everything (which
//! would be a working allow).

use std::net::{IpAddr, Ipv4Addr, Ipv6Addr};

use crate::error::{Error, Result};

/// One parsed host pattern.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) enum Pattern {
    /// A domain and (optionally) its subdomains.
    Domain {
        /// Lower-case, no trailing dot.
        name: String,
        /// Whether `name` itself matches, or only names under it.
        include_apex: bool,
    },
    /// One address.
    Ip(IpAddr),
    /// An address block.
    Cidr {
        /// The network address.
        base: IpAddr,
        /// Prefix length in bits.
        bits: u8,
    },
}

impl Pattern {
    /// Parse one entry.
    pub(crate) fn parse(raw: &str) -> Result<Self> {
        let raw = raw.trim();
        if raw.is_empty() {
            return Err(Error::validation("sandbox: net rule host is empty"));
        }
        if raw.contains('/') {
            return Self::parse_cidr(raw);
        }
        let host = strip_brackets(raw);
        if let Ok(ip) = host.parse::<IpAddr>() {
            return Ok(Self::Ip(ip));
        }
        let (include_apex, name) = match host.strip_prefix("*.") {
            Some(rest) => (false, rest),
            None => (true, host),
        };
        if name.is_empty() || name.starts_with('.') || name.contains("..") {
            return Err(Error::validation(format!(
                "sandbox: net rule host {raw:?} is not a domain"
            )));
        }
        if name.contains('*') {
            return Err(Error::validation(format!(
                "sandbox: net rule host {raw:?}: '*' is only allowed as a leading '*.'"
            )));
        }
        let name = name.trim_end_matches('.').to_ascii_lowercase();
        if name.is_empty() {
            return Err(Error::validation(format!(
                "sandbox: net rule host {raw:?} is not a domain"
            )));
        }
        Ok(Self::Domain { name, include_apex })
    }

    fn parse_cidr(raw: &str) -> Result<Self> {
        let (address, bits) = raw.split_once('/').ok_or_else(|| {
            Error::validation(format!(
                "sandbox: net rule host {raw:?} is not a CIDR block"
            ))
        })?;
        let base: IpAddr = strip_brackets(address).parse().map_err(|_| {
            Error::validation(format!(
                "sandbox: net rule host {raw:?}: {address:?} is not an address"
            ))
        })?;
        let bits: u8 = bits.parse().map_err(|_| {
            Error::validation(format!("sandbox: net rule host {raw:?}: bad prefix length"))
        })?;
        let max = if base.is_ipv4() { 32 } else { 128 };
        if bits > max {
            return Err(Error::validation(format!(
                "sandbox: net rule host {raw:?}: prefix {bits} exceeds {max}"
            )));
        }
        Ok(Self::Cidr { base, bits })
    }

    /// Whether this pattern names an address rather than a name.
    pub(crate) fn is_ip(&self) -> bool {
        !matches!(self, Self::Domain { .. })
    }

    /// Whether a hostname matches (domain patterns only).
    pub(crate) fn matches_host(&self, host: &str) -> bool {
        let Self::Domain { name, include_apex } = self else {
            return false;
        };
        let host = host.trim_end_matches('.').to_ascii_lowercase();
        if *include_apex && host == *name {
            return true;
        }
        // `example.com` must not match `notexample.com`.
        host.len() > name.len()
            && host.ends_with(name.as_str())
            && host.as_bytes()[host.len() - name.len() - 1] == b'.'
    }

    /// Whether an address matches (address patterns only).
    pub(crate) fn matches_ip(&self, ip: IpAddr) -> bool {
        // `::ffff:10.1.2.3` reaches 10.1.2.3, so a v4 rule has to judge the
        // canonical form: otherwise the mapped spelling walks past a deny.
        let ip = ip.to_canonical();
        match self {
            Self::Domain { .. } => false,
            Self::Ip(pattern) => *pattern == ip,
            Self::Cidr { base, bits } => match (base, ip) {
                (IpAddr::V4(base), IpAddr::V4(ip)) => in_v4_block(*base, ip, *bits),
                (IpAddr::V6(base), IpAddr::V6(ip)) => in_v6_block(*base, ip, *bits),
                // A v4 rule never matches a v6 destination; the caller resolves
                // both families and each answer is checked on its own.
                _ => false,
            },
        }
    }

    /// The spelling used in a decision record.
    pub(crate) fn describe(&self) -> String {
        match self {
            Self::Domain { name, include_apex } => {
                if *include_apex {
                    name.clone()
                } else {
                    format!("*.{name}")
                }
            }
            Self::Ip(ip) => ip.to_string(),
            Self::Cidr { base, bits } => format!("{base}/{bits}"),
        }
    }
}

fn strip_brackets(raw: &str) -> &str {
    raw.strip_prefix('[')
        .and_then(|rest| rest.strip_suffix(']'))
        .unwrap_or(raw)
}

fn in_v4_block(base: Ipv4Addr, ip: Ipv4Addr, bits: u8) -> bool {
    if bits == 0 {
        return true;
    }
    let mask = u32::MAX << (32 - u32::from(bits));
    u32::from(base) & mask == u32::from(ip) & mask
}

fn in_v6_block(base: Ipv6Addr, ip: Ipv6Addr, bits: u8) -> bool {
    if bits == 0 {
        return true;
    }
    let mask = u128::MAX << (128 - u32::from(bits));
    u128::from(base) & mask == u128::from(ip) & mask
}

#[cfg(test)]
mod tests {
    use super::*;

    fn pattern(raw: &str) -> Pattern {
        Pattern::parse(raw).expect("pattern")
    }

    #[test]
    fn a_bare_domain_covers_itself_and_its_subdomains() {
        let domain = pattern("example.com");
        assert!(domain.matches_host("example.com"));
        assert!(domain.matches_host("api.example.com"));
        assert!(domain.matches_host("a.b.example.com"));
        assert!(domain.matches_host("EXAMPLE.COM"));
        assert!(domain.matches_host("example.com."));
        assert!(!domain.matches_host("notexample.com"));
        assert!(!domain.matches_host("example.com.evil"));
    }

    #[test]
    fn a_wildcard_covers_only_subdomains() {
        let wildcard = pattern("*.example.com");
        assert!(wildcard.matches_host("api.example.com"));
        assert!(wildcard.matches_host("a.b.example.com"));
        assert!(!wildcard.matches_host("example.com"), "never the apex");
    }

    #[test]
    fn an_address_literal_matches_exactly_itself() {
        let literal = pattern("1.2.3.4");
        assert!(literal.is_ip());
        assert!(literal.matches_ip("1.2.3.4".parse().unwrap()));
        assert!(!literal.matches_ip("1.2.3.5".parse().unwrap()));
        assert!(
            !literal.matches_host("1.2.3.4"),
            "an IP pattern is not a host rule"
        );
    }

    #[test]
    fn a_cidr_block_is_inclusive_of_its_edges() {
        let private = pattern("10.0.0.0/8");
        assert!(private.matches_ip("10.0.0.0".parse().unwrap()));
        assert!(private.matches_ip("10.255.255.255".parse().unwrap()));
        assert!(!private.matches_ip("11.0.0.0".parse().unwrap()));

        let subnet = pattern("192.168.1.0/24");
        assert!(subnet.matches_ip("192.168.1.255".parse().unwrap()));
        assert!(!subnet.matches_ip("192.168.2.1".parse().unwrap()));
    }

    #[test]
    fn a_v6_block_is_matched_too() {
        let block = pattern("fd00::/8");
        assert!(block.matches_ip("fd12::1".parse().unwrap()));
        assert!(!block.matches_ip("fe80::1".parse().unwrap()));
        let loopback = pattern("[::1]");
        assert!(loopback.matches_ip("::1".parse().unwrap()));
    }

    #[test]
    fn a_v4_rule_never_matches_a_v6_destination() {
        let pattern = pattern("127.0.0.1");
        assert!(!pattern.matches_ip("::1".parse().unwrap()));
    }

    #[test]
    fn an_ipv4_mapped_literal_is_judged_as_the_v4_address() {
        // `::ffff:10.1.2.3` is routed to 10.1.2.3, so a deny must catch it.
        let denied = pattern("10.0.0.0/8");
        assert!(denied.matches_ip("::ffff:10.1.2.3".parse().unwrap()));
        let loopback = pattern("127.0.0.1");
        assert!(loopback.matches_ip("::ffff:127.0.0.1".parse().unwrap()));
    }

    #[test]
    fn malformed_patterns_are_rejected_at_build_time() {
        for raw in [
            "",
            " ",
            "a..b",
            ".example.com",
            "*",
            "*.*.com",
            "example.com/*",
            "1.2.3.4/33",
            "1.2.3.4/x",
        ] {
            assert!(
                Pattern::parse(raw).is_err(),
                "{raw:?} must not compile into a silent no-op rule"
            );
        }
    }

    #[test]
    fn a_pattern_describes_itself_for_the_audit_record() {
        assert_eq!(pattern("example.com").describe(), "example.com");
        assert_eq!(pattern("*.example.com").describe(), "*.example.com");
        assert_eq!(pattern("10.0.0.0/8").describe(), "10.0.0.0/8");
    }
}
