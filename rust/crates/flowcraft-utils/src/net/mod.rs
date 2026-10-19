//! `core/utils/net`: what an egress pattern means, and the proxy that enforces
//! it.
//!
//! A sandbox's network posture is a list of rules, and a rule's host is a
//! pattern: `example.com` is the domain *and every subdomain*, `*.example.com`
//! is the subdomains only, `1.2.3.4` and `10.0.0.0/8` are addresses. Which of
//! those a string is, and which names it then covers, is decided here -- [`host`]
//! compiles and matches, [`matcher`] applies the compiled rules in core's order,
//! and [`policy`] holds the data both read.
//!
//! # The two halves, and which one pins what
//!
//! The **decision** is a pure function of its input: [`host::compile`],
//! [`matcher::Matcher::match_host`], [`policy::NetPolicy::validate`],
//! [`proxy::Proxy::decide`] and [`mitm::Selection::should_mitm`] do no I/O, so
//! `rust/conformance/net-contract.json` and `proxy-contract.json` hold every
//! answer they give to a byte.
//!
//! The **wire** is not pure, so it is pinned differently: `socks5` frames
//! RFC 1928 in both directions, [`dial`] owns the three shapes a connection can
//! take, and [`serve`] owns the listener that multiplexes SOCKS5 and HTTP on one
//! address. `rust/conformance/wire-contract.json` records what core's wire does
//! with the same inputs -- the reply bytes, the audit record they produce, and
//! the bytes an upstream receives -- and `tests/wire_contract.rs` replays them.
//!
//! Under that wire sits a third: [`mitm::Engine`] terminates the TLS a policy
//! says to terminate and forwards what it finds underneath. Its contract is
//! `rust/conformance/mitm-contract.json` -- the handshake the engine offers, and
//! the three views one tunnel has (what the hooks were told, what the client got
//! back, what the target received) -- replayed by `tests/mitm_contract.rs` for
//! the handshakes and `tests/mitm_sessions.rs` for the traffic.
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
//!   package-level `net.DefaultResolver`; the serving proxy here uses
//!   [`proxy::SystemResolver`], and the trait is what lets the address pass be
//!   tested -- and contract-pinned -- without a network.
//! - A domain name on the SOCKS5 wire that is not UTF-8 becomes U+FFFD, where
//!   core keeps the bytes and lets the dial fail. Both end as a failed dial; the
//!   difference is a name in a log line.
//! - The HTTP side is a seam, not a parser: [`serve::HttpHandler`] is what an
//!   HTTP stack plugs into, and [`serve::Server::handle_connect`] is the whole
//!   `CONNECT` path once a request has been parsed. Core hands both to
//!   `net/http`.
//! - [`mitm::Engine`] takes the roots it verifies targets with as a parameter
//!   where core fills in the system pool from a `nil`. Loading the platform
//!   store is the deploy slice's job; a run inside a sandbox pins one.
//! - A pooled upstream connection that turns out to be dead answers `502`;
//!   core's transport retries an idempotent request on a fresh connection. A
//!   retry needs the body, which is a stream by then, so the port reports the
//!   failure instead of replaying it.
//! - The h2 branch of [`mitm::Engine::serve`] drops the error its server
//!   returns, which is what core's does: `serveH2` runs the session in
//!   `http.Server`'s goroutine, so a client that vanishes has no caller to
//!   tell. The h1 branch reports everything that is not a closed connection.

use std::future::Future;
use std::pin::Pin;

pub mod addr;
pub mod dial;
pub mod host;
pub mod matcher;
pub mod mitm;
pub mod policy;
pub mod proxy;
pub mod serve;
pub mod socks5;
pub mod url;

/// A boxed future, for the traits that have to be object-safe.
pub type BoxFuture<'a, T> = Pin<Box<dyn Future<Output = T> + Send + 'a>>;

pub use addr::{Addr, Prefix};
pub use dial::{Dialer, Upstream};
pub use host::{Pattern, Set};
pub use matcher::{Decision, Matcher};
pub use mitm::{Engine, Hooks, Selection};
pub use policy::{MitmPolicy, NetAction, NetMode, NetPolicy, NetRule};
pub use proxy::{Audit, Proxy, ProxyConfig, Resolver, SystemResolver, Verdict};
pub use serve::{Bound, HttpHandler, Outcome, Refusal, Route, Server};
pub use socks5::{Credentials, Dialed, Stream, Target};
