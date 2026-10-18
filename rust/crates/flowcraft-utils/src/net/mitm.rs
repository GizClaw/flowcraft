//! `net/mitm`: which CONNECT tunnels are terminated.
//!
//! MITM is opt-in and narrow: a policy may list the hosts worth decrypting and
//! the hosts that never are. [`Selection`] is that decision, compiled once, and
//! it is the only part of core's `mitm` package that is a function of its
//! input -- the rest is a certificate authority and a TLS server.
//!
//! The selection's two rules are core's:
//!
//! - an **exclude** always wins, so a host can be taken out of a range without
//!   narrowing the range;
//! - an **empty include list means every host**, while a non-empty one means
//!   only what it names. That is the opposite of the empty-set convention used
//!   everywhere else in this package, and it is why the check tests `is_empty`
//!   rather than just matching.
//!
//! A host that is an IP literal is matched as an address, so `10.0.0.0/8` in
//! `hosts` intercepts `10.1.2.3`; a name is matched as a name. Both go through
//! [`Set`], which is the same pattern grammar the rule set uses.
//!
//! # The seam, and what it will be
//!
//! Core terminates TLS through an injected `MITMEngine`, built by a
//! `MITMFactory` from the policy, the hooks, the proxy's policy-aware dialer
//! and the outbound roots. The port keeps that seam -- the proxy will ask
//! [`Selection::should_mitm`] and hand the tunnel to an engine -- but the
//! engine's *shape* is not here: `serve` takes a client connection, and the
//! connection type belongs to the server slice. Writing the trait before the
//! server exists would mean guessing that type and rewriting it.
//!
//! That slice also brings the hooks (`OnConnect`, `OnRequest`, `OnResponse` in
//! core) and the certificate authority, neither of which is worth a type
//! without a TLS stack behind it.

use flowcraft_contract::error::{Error, ErrorKind};

use super::addr::Addr;
use super::host::Set;
use super::policy::MitmPolicy;

/// The compiled form of a [`MitmPolicy`]'s host selection.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Selection {
    includes: Set,
    excludes: Set,
}

impl Selection {
    /// Compile a policy's selection. An unparsable pattern is refused with
    /// core's sentence: the include list first, then the exclude list, which is
    /// the order `mitm.New` reports them in.
    ///
    /// Core's `mitm.New` returns a bare error for either list; here it is
    /// classified as [`ErrorKind::Validation`], because a pattern that does not
    /// compile is a configuration refusal.
    pub fn new(policy: &MitmPolicy) -> Result<Selection, Error> {
        let includes = Set::new(&policy.hosts)
            .map_err(|err| Error::new(ErrorKind::Validation, format!("mitm: hosts: {err}")))?;
        let excludes = Set::new(&policy.exclude_hosts).map_err(|err| {
            Error::new(ErrorKind::Validation, format!("mitm: exclude_hosts: {err}"))
        })?;
        Ok(Selection { includes, excludes })
    }

    /// `Engine.ShouldMITM`: is this host terminated, or tunnelled through?
    ///
    /// Excludes win; an empty include list means all hosts; anything else has
    /// to be named.
    pub fn should_mitm(&self, host: &str) -> bool {
        if matches(&self.excludes, host) {
            return false;
        }
        if !self.includes.is_empty() && !matches(&self.includes, host) {
            return false;
        }
        true
    }
}

/// Match one host against a set, sending an IP literal through the address
/// path so a CIDR entry in `hosts` means what it says.
fn matches(set: &Set, host: &str) -> bool {
    match Addr::parse(host) {
        Some(address) => set.matches_ip(&address.unmap()),
        None => set.matches_host(host),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn selection(hosts: &[&str], excludes: &[&str]) -> Selection {
        match Selection::new(&policy(hosts, excludes)) {
            Ok(selection) => selection,
            Err(err) => panic!("the selection should compile: {err}"),
        }
    }

    fn policy(hosts: &[&str], excludes: &[&str]) -> MitmPolicy {
        MitmPolicy {
            enabled: true,
            hosts: hosts.iter().map(|host| (*host).to_owned()).collect(),
            exclude_hosts: excludes.iter().map(|host| (*host).to_owned()).collect(),
            ..MitmPolicy::default()
        }
    }

    #[test]
    fn an_empty_include_list_means_every_host() {
        let selection = selection(&[], &[]);
        assert!(selection.should_mitm("example.com"));
        assert!(selection.should_mitm("10.1.2.3"));
    }

    #[test]
    fn an_exclude_wins_over_an_include() {
        let selection = selection(&["example.com"], &["*.example.com"]);
        assert!(selection.should_mitm("example.com"));
        assert!(!selection.should_mitm("www.example.com"));
    }

    #[test]
    fn an_address_pattern_intercepts_an_address_query() {
        let selection = selection(&["10.0.0.0/8"], &[]);
        assert!(selection.should_mitm("10.1.2.3"));
        assert!(!selection.should_mitm("127.0.0.1"));
    }

    #[test]
    fn a_pattern_that_does_not_compile_names_the_list_it_came_from() {
        let policy = policy(&["example.com"], &["*"]);
        let err = match Selection::new(&policy) {
            Ok(_) => panic!("the exclude list should be refused"),
            Err(err) => err,
        };
        assert_eq!(
            err.to_string(),
            "mitm: exclude_hosts: host \"*\": \"*\" is only allowed as a leading \"*.\" wildcard"
        );
    }
}
