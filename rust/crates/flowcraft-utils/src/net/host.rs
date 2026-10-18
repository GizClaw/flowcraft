//! Host patterns: compile once, match many, resolve nothing.
//!
//! Four shapes, and the shape decides the answer:
//!
//! - `example.com` -- the bare domain *and every subdomain*. The subdomain
//!   reading is core's, matching the legacy `AllowHosts` grammar every existing
//!   configuration was written against.
//! - `*.example.com` -- subdomains only, any depth, never the bare domain.
//! - `1.2.3.4` -- an address, compared against resolved addresses and never
//!   against hostnames.
//! - `10.0.0.0/8` -- a range, same.
//!
//! A string that is none of those is still a valid pattern: it becomes a
//! hostname compared as written. That is why `10.0.0.0/33` -- a prefix core
//! would not parse -- is a name that matches nothing, rather than an error. The
//! refusals are narrower: an empty pattern, and a `*` anywhere but the front
//! (see [`Error`]).
//!
//! # Normalization happens on both sides
//!
//! [`compile`] normalizes the pattern, and [`Pattern::matches_host`] normalizes
//! the name it is asked about, so `Example.COM.` written in a policy matches
//! `example.com` asked at runtime. [`normalize`] is the function, and it is
//! where the IDNA decision lives: unicode names become punycode, and names
//! `idna.Lookup` refuses are compared as written instead. Core's own
//! normalization in [`crate::net::matcher`] is the same function minus IDNA,
//! for the same reason (a hostname that arrives over the wire has already been
//! through whatever conversion its client did).

use flowcraft_contract::text;
use idna::uts46::{AsciiDenyList, DnsLength, Hyphens, Uts46};

use super::addr::{Addr, Prefix};

/// The refusals [`compile`] makes.
#[derive(Debug, Clone, PartialEq, Eq)]
#[non_exhaustive]
pub enum Error {
    /// The pattern was empty or only whitespace.
    EmptyHost,
    /// A `*` that is not the leading `*.` of a wildcard.
    MisplacedWildcard {
        /// The pattern as the caller wrote it.
        raw: String,
    },
    /// A leading `*.` with nothing usable after it, or a second `*`.
    MalformedWildcard {
        /// The pattern as the caller wrote it.
        raw: String,
    },
}

impl std::fmt::Display for Error {
    /// core's sentences, with Go's `%q` spelling of the pattern.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Error::EmptyHost => f.write_str("empty host"),
            Error::MisplacedWildcard { raw } => write!(
                f,
                "host {}: \"*\" is only allowed as a leading \"*.\" wildcard",
                text::quote(raw)
            ),
            Error::MalformedWildcard { raw } => write!(
                f,
                "wildcard host {} must have form \"*.example.com\"",
                text::quote(raw)
            ),
        }
    }
}

/// One compiled host pattern.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Pattern {
    /// A domain and its descendants.
    Domain {
        /// The normalized domain.
        host: String,
    },
    /// Subdomains of a domain, any depth, never the domain itself.
    Wildcard {
        /// The normalized domain whose subdomains match.
        host: String,
    },
    /// One address.
    Addr(Addr),
    /// A range of addresses.
    Prefix(Prefix),
}

impl Pattern {
    /// `Pattern.IsIP`: does this pattern need resolved addresses to answer?
    ///
    /// True for the address and range shapes, false for the two name shapes.
    pub fn is_ip(&self) -> bool {
        matches!(self, Pattern::Addr(_) | Pattern::Prefix(_))
    }

    /// `Pattern.Match`: does this pattern cover the hostname?
    ///
    /// A name is never compared against an address pattern: `127.0.0.1` in a
    /// rule does not match the *name* `127.0.0.1`, because the name may resolve
    /// anywhere and the rule is about where it lands.
    pub fn matches_host(&self, host: &str) -> bool {
        let normalized = normalize(host);
        match self {
            Pattern::Domain { host: pattern } => {
                normalized == *pattern || within_domain(&normalized, pattern)
            }
            Pattern::Wildcard { host: pattern } => within_domain(&normalized, pattern),
            Pattern::Addr(_) | Pattern::Prefix(_) => false,
        }
    }

    /// `Pattern.MatchIP`: does this pattern cover the address?
    ///
    /// The address is unmapped first, so a client that came in through the
    /// IPv4-mapped form is judged by the IPv4 address it wraps.
    pub fn matches_ip(&self, ip: &Addr) -> bool {
        let ip = ip.unmap();
        match self {
            Pattern::Addr(pattern) => ip == *pattern,
            Pattern::Prefix(prefix) => prefix.contains(&ip),
            Pattern::Domain { .. } | Pattern::Wildcard { .. } => false,
        }
    }
}

/// `strings.HasSuffix(host, "."+pattern)`: a strict subdomain, at any depth.
fn within_domain(host: &str, pattern: &str) -> bool {
    host.len() > pattern.len()
        && host.ends_with(pattern)
        && host.as_bytes()[host.len() - pattern.len() - 1] == b'.'
}

/// Compile one pattern, or say why it cannot be one.
pub fn compile(raw: &str) -> Result<Pattern, Error> {
    let host = raw.trim();
    if host.is_empty() {
        return Err(Error::EmptyHost);
    }
    if host.contains('*') && !host.starts_with("*.") {
        return Err(Error::MisplacedWildcard {
            raw: raw.to_owned(),
        });
    }
    let (wild, host) = match host.strip_prefix("*.") {
        Some(rest) => {
            if rest.is_empty() || rest.contains('*') {
                return Err(Error::MalformedWildcard {
                    raw: raw.to_owned(),
                });
            }
            (true, rest)
        }
        None => (false, host),
    };
    let host = normalize(host);
    if !wild {
        // An address or a range is decided by what parses; everything else is
        // a name, including the near misses (`1.2.3`, `10.0.0.0/33`).
        if let Some(addr) = Addr::parse(&host) {
            return Ok(Pattern::Addr(addr.unmap()));
        }
        if let Some(prefix) = Prefix::parse(&host) {
            return Ok(Pattern::Prefix(prefix));
        }
    }
    Ok(if wild {
        Pattern::Wildcard { host }
    } else {
        Pattern::Domain { host }
    })
}

/// `hostmatch.normalize`: trim, lowercase, drop one trailing dot, then IDNA.
///
/// The IDNA step runs only for names that are still non-ASCII afterwards, and
/// only for names [`Uts46`] accepts under the rules core's `idna.Lookup`
/// profile uses: STD3 (letters, digits, hyphen and dot only), hyphen position
/// checks, the bidi and joiner rules, and no DNS length limit. A name those
/// rules refuse is returned as written, because a pattern that cannot be
/// converted is still a pattern -- it just matches only its own spelling.
pub fn normalize(host: &str) -> String {
    let lowered = lower_simple(host.trim());
    let host = lowered.strip_suffix('.').unwrap_or(&lowered);
    if host.is_ascii() {
        return host.to_owned();
    }
    match Uts46::new().to_ascii(
        host.as_bytes(),
        AsciiDenyList::STD3,
        Hyphens::Check,
        DnsLength::Ignore,
    ) {
        Ok(ascii) => ascii.into_owned(),
        Err(_) => host.to_owned(),
    }
}

/// Go's `strings.ToLower`, which is a *simple* case mapping: one character in,
/// one character out.
///
/// Rust's `char::to_lowercase` is the full mapping, and it expands for exactly
/// one character in Unicode 16: U+0130 (`İ`) becomes `i` + U+0307. Go lowers it
/// to a single `i`. Taking the first character of the expansion reproduces
/// Go's mapping for every character -- the table has one entry, so this is the
/// whole of the difference, and `rust/conformance/net-contract.json` pins it
/// with an `İstanbul.example` row.
fn lower_simple(text: &str) -> String {
    let mut lowered = String::with_capacity(text.len());
    for character in text.chars() {
        if let Some(first) = character.to_lowercase().next() {
            lowered.push(first);
        }
    }
    lowered
}

/// An ordered collection of patterns; a host matches if any pattern does.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Set {
    patterns: Vec<Pattern>,
}

impl Set {
    /// Compile a host list. An empty list is an empty set, which matches
    /// nothing; one bad entry fails the whole set with that entry's error.
    pub fn new(hosts: &[String]) -> Result<Set, Error> {
        let mut patterns = Vec::with_capacity(hosts.len());
        for raw in hosts {
            patterns.push(compile(raw)?);
        }
        Ok(Set { patterns })
    }

    /// `Set.Empty`: no patterns, so no match.
    pub fn is_empty(&self) -> bool {
        self.patterns.is_empty()
    }

    /// Does any pattern cover the hostname?
    pub fn matches_host(&self, host: &str) -> bool {
        self.patterns
            .iter()
            .any(|pattern| pattern.matches_host(host))
    }

    /// Does any address pattern cover the address?
    pub fn matches_ip(&self, ip: &Addr) -> bool {
        self.patterns.iter().any(|pattern| pattern.matches_ip(ip))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The refusal, which every caller of this in the tests expects.
    fn refused(result: Result<Pattern, Error>) -> Error {
        match result {
            Ok(_) => panic!("the pattern should have been refused"),
            Err(err) => err,
        }
    }

    fn pattern(raw: &str) -> Pattern {
        match compile(raw) {
            Ok(pattern) => pattern,
            Err(err) => panic!("{raw:?}: {err}"),
        }
    }

    #[test]
    fn a_domain_matches_itself_and_its_descendants() {
        let domain = pattern("example.com");
        assert!(domain.matches_host("example.com"));
        assert!(domain.matches_host("www.example.com"));
        assert!(domain.matches_host("a.b.example.com"));
        assert!(domain.matches_host("Example.COM."));
        assert!(!domain.matches_host("notexample.com"));
        assert!(!domain.matches_host("example.com.evil"));
        assert!(!domain.matches_host("example.co"));
    }

    #[test]
    fn a_wildcard_never_matches_the_bare_domain() {
        let wildcard = pattern("*.example.com");
        assert!(!wildcard.matches_host("example.com"));
        assert!(wildcard.matches_host("www.example.com"));
        assert!(wildcard.matches_host("a.b.example.com"));
        assert!(!wildcard.matches_host("example.com.evil"));
    }

    #[test]
    fn an_address_pattern_ignores_names_and_a_name_pattern_ignores_addresses() {
        let address = pattern("10.0.0.1");
        assert!(address.is_ip());
        assert!(!address.matches_host("10.0.0.1"));
        assert!(address.matches_ip(&addr("10.0.0.1")));
        assert!(address.matches_ip(&addr("::ffff:10.0.0.1")));

        let domain = pattern("example.com");
        assert!(!domain.is_ip());
        assert!(!domain.matches_ip(&addr("10.0.0.1")));
    }

    #[test]
    fn what_does_not_parse_as_an_address_is_a_name() {
        for raw in ["1.2.3", "1.2.3.4.5", "010.1.2.3", "10.0.0.0/33", "::1::2"] {
            let compiled = pattern(raw);
            assert!(!compiled.is_ip(), "{raw:?}");
            assert!(compiled.matches_host(raw), "{raw:?} matches itself");
        }
    }

    #[test]
    fn refusals_carry_cores_sentences() {
        assert_eq!(
            refused(compile("")).to_string(),
            "empty host",
            "an empty pattern has no leading or trailing space to quote"
        );
        assert_eq!(
            refused(compile("*")).to_string(),
            "host \"*\": \"*\" is only allowed as a leading \"*.\" wildcard"
        );
        assert_eq!(
            refused(compile("a.*.com")).to_string(),
            "host \"a.*.com\": \"*\" is only allowed as a leading \"*.\" wildcard"
        );
        assert_eq!(
            refused(compile("*.")).to_string(),
            "wildcard host \"*.\" must have form \"*.example.com\""
        );
        assert_eq!(
            refused(compile("*.*.example.com")).to_string(),
            "wildcard host \"*.*.example.com\" must have form \"*.example.com\""
        );
    }

    #[test]
    fn normalization_is_what_makes_the_two_sides_meet() {
        assert_eq!(normalize(" Example.COM. "), "example.com");
        // One trailing dot only: the second one stays, and then the pattern
        // matches that literal spelling and nothing else.
        assert_eq!(normalize("example.com.."), "example.com.");
        // IDNA turns a unicode name into punycode...
        assert_eq!(normalize("例え.テスト"), "xn--r8jz45g.xn--zckzah");
        assert_eq!(normalize("ЕХАМПЛЕ.РФ"), "xn--80ajbxes3b.xn--p1ai");
        // ...including the deviation character, non-transitional: `ß` is not
        // `ss`.
        assert_eq!(normalize("faß.de"), "xn--fa-hia.de");
        // A name the rules refuse is compared as written, lowercased.
        assert_eq!(normalize("a_b.example.com"), "a_b.example.com");
        assert_eq!(normalize("A_B.example.com"), "a_b.example.com");
    }

    #[test]
    fn the_one_character_where_full_and_simple_lowercasing_differ() {
        // Go's strings.ToLower maps U+0130 to a single `i`; Rust's expands it
        // to `i` + U+0307. The simple mapping wins, and the result is ASCII,
        // so IDNA never sees it.
        assert_eq!(normalize("İstanbul.example"), "istanbul.example");
        assert_eq!(lower_simple("İ"), "i");
    }

    #[test]
    fn a_set_matches_when_any_pattern_does() {
        let set = match Set::new(&[
            "example.com".to_owned(),
            "*.cdn.example".to_owned(),
            "10.0.0.0/8".to_owned(),
        ]) {
            Ok(set) => set,
            Err(err) => panic!("{err}"),
        };
        assert!(!set.is_empty());
        assert!(set.matches_host("example.com"));
        assert!(set.matches_host("img.cdn.example"));
        assert!(!set.matches_host("cdn.example"));
        assert!(set.matches_ip(&addr("10.1.2.3")));
        assert!(!set.matches_ip(&addr("11.1.2.3")));

        let empty = Set::default();
        assert!(empty.is_empty());
        assert!(!empty.matches_host("example.com"));
        assert!(!empty.matches_ip(&addr("10.1.2.3")));
    }

    #[test]
    fn a_set_fails_with_its_first_bad_entry() {
        let err = match Set::new(&["example.com".to_owned(), "*bad.example".to_owned()]) {
            Ok(_) => panic!("a bad entry should fail the set"),
            Err(err) => err,
        };
        assert_eq!(
            err.to_string(),
            "host \"*bad.example\": \"*\" is only allowed as a leading \"*.\" wildcard"
        );
    }

    fn addr(text: &str) -> Addr {
        match Addr::parse(text) {
            Some(addr) => addr,
            None => panic!("{text:?} parses"),
        }
    }
}
