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
//! The slice lands in two pieces. [`ca`] is the authority -- the root a run
//! mints, and the leaves it signs per host -- and it is testable without a
//! socket, because a handshake can be two in-memory `rustls` connections.
//! [`engine`] is what sits on that authority: the terminated tunnel, the hooks
//! (`OnConnect`, `OnRequest`, `OnResponse` in core), and the forwarding
//! transport.
//!
//! What both pieces are held to is committed: `rust/conformance/mitm-contract.json`
//! pins the handshake the authority offers and, through the engine, the
//! decrypted traffic -- what hooks see, what the client gets back, what the
//! target receives. `tests/mitm_contract.rs` replays the first;
//! `tests/mitm_sessions.rs` the second.

use flowcraft_contract::error::{Error, ErrorKind};

use super::addr::Addr;
use super::host::Set;
use super::policy::MitmPolicy;

pub mod ca;
pub mod engine;

pub use engine::{Engine, RequestInfo, ResponseInfo, UpstreamDialer};

/// Core's `MITMHooks`, in the shape the port's two halves can both use.
///
/// Core's interface has three methods because Go has no defaults; here the two
/// the engine calls are defaulted, so a proxy that only wants to watch `CONNECT`
/// writes [`Hooks::on_connect`] and a backend that only wants content hooks
/// writes the other two.
///
/// The three are one trait rather than three because they are one policy: a
/// hook that refuses a host at `CONNECT` and a hook that refuses its requests
/// are decided by the same configuration, and a caller that installs one almost
/// always installs the other.
pub trait Hooks: Send + Sync {
    /// Called once the policy allowed a destination, before it is dialed. A
    /// refusal blocks the connection.
    ///
    /// The sentence is handed back, not written: core's `onConnect` returns an
    /// `error` whose text becomes the refusal, and the HTTP side surfaces it
    /// where a `CONNECT` is answered.
    fn on_connect(&self, _host: &str, _port: i32) -> Result<(), String> {
        Ok(())
    }

    /// Called with every decrypted request before it is forwarded. A refusal is
    /// answered `403` with core's sentence.
    fn on_request(&self, _info: &RequestInfo) -> Result<(), Error> {
        Ok(())
    }

    /// Called with every decrypted response before it is written back. A
    /// refusal is answered `403` with core's sentence.
    fn on_response(&self, _info: &ResponseInfo) -> Result<(), Error> {
        Ok(())
    }
}

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
