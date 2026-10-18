//! `core/utils/net`: what an egress pattern means.
//!
//! A sandbox's network posture is a list of rules, and a rule's host is a
//! pattern: `example.com` is the domain *and every subdomain*, `*.example.com`
//! is the subdomains only, `1.2.3.4` and `10.0.0.0/8` are addresses. Which of
//! those a string is, and which names it then covers, is decided here --
//! [`host`] compiles and matches, [`matcher`] applies the compiled rules in
//! core's order, and [`policy`] holds the data both read.
//!
//! This is the half of the Go package that is a pure function of its input:
//! [`host::compile`], [`host::Pattern::matches_host`] and
//! [`matcher::Matcher::match_host`] do no I/O and no DNS, so
//! `rust/conformance/net-contract.json` holds every one of their answers to a
//! byte. The other half -- the host-side proxy that listens, multiplexes HTTP
//! and SOCKS5 and tunnels -- is sockets and state machines, and lands in its own
//! slice.
//!
//! # What a caller can tell apart from core
//!
//! - `NetPolicy.Validate` is not here. Every branch but one is a
//!   range or a trim; the remaining one is `url.Parse`, which core leans on to
//!   decide whether a proxy URL has a scheme it can use, a host, and a password
//!   without a user. That grammar is a slice of its own, and a validation that
//!   skipped it would accept a policy core refuses -- worse than not having the
//!   check yet. The proxy slice brings both.
//! - A rule's port is an `i32` where core has Go's `int`. The valid range is
//!   `0..=65535`, which `i32` covers; the difference is only visible on inputs
//!   `NetPolicy.Validate` would refuse anyway.
//! - [`policy::NetAction`] has two variants and no third. Core's `String` says
//!   `"unknown"` for a value outside `NetDeny`/`NetAllow`; that value cannot be
//!   constructed here.
//! - Non-ASCII patterns go through IDNA exactly where core's do (see
//!   [`host::normalize`]) and nowhere else, including the one character where
//!   Rust's lowercasing and Go's disagree.

pub mod addr;
pub mod host;
pub mod matcher;
pub mod policy;

pub use addr::{Addr, Prefix};
pub use host::{Pattern, Set};
pub use matcher::{Decision, Matcher};
pub use policy::{MitmPolicy, NetAction, NetMode, NetPolicy, NetRule};
