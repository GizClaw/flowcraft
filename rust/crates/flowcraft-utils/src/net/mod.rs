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
//! byte. The proxy's policy half is here too: [`policy::NetPolicy::validate`],
//! [`proxy::Proxy::decide`] and [`mitm::Selection::should_mitm`] answer what the
//! serving proxy would do without serving anything, and
//! `rust/conformance/proxy-contract.json` pins them.
//!
//! The other half -- the host-side proxy that listens, multiplexes HTTP and
//! SOCKS5 and tunnels, and the TLS engine behind MITM -- is sockets and state
//! machines, and lands in its own slice.
//!
//! # What a caller can tell apart from core
//!
//! - Two `NetPolicy.Validate` branches are unreachable: an unknown [`NetMode`]
//!   and an unknown [`NetAction`] cannot be constructed, so the checks that
//!   refuse them are always satisfied. Every other refusal is here, including
//!   the URL grammar ([`url`]) the proxy settings are read with.
//! - A rule's port is an `i32` where core has Go's `int`. The valid range is
//!   `0..=65535`, which `i32` covers; the difference is only visible on inputs
//!   `NetPolicy.Validate` would refuse anyway.
//! - [`policy::NetAction`] has two variants and no third. Core's `String` says
//!   `"unknown"` for a value outside `NetDeny`/`NetAllow`; that value cannot be
//!   constructed here.
//! - Non-ASCII patterns go through IDNA exactly where core's do (see
//!   [`host::normalize`]) and nowhere else, including the one character where
//!   Rust's lowercasing and Go's disagree.
//! - [`proxy::Proxy`] takes its resolver as a trait object. Core reads a
//!   package-level `net.DefaultResolver`; the serving proxy here will use
//!   [`proxy::SystemResolver`], and the trait is what lets the address pass be
//!   tested -- and contract-pinned -- without a network.

pub mod addr;
pub mod host;
pub mod matcher;
pub mod mitm;
pub mod policy;
pub mod proxy;
pub mod url;

pub use addr::{Addr, Prefix};
pub use host::{Pattern, Set};
pub use matcher::{Decision, Matcher};
pub use mitm::Selection;
pub use policy::{MitmPolicy, NetAction, NetMode, NetPolicy, NetRule};
pub use proxy::{Audit, Proxy, ProxyConfig, Resolver, SystemResolver, Verdict};
