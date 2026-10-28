//! Network policy enforcement.
//!
//! Three layers, and the boundary between them is the whole design:
//!
//! - [`pattern`] turns one rule's host into something matchable;
//! - [`matcher`] answers one question — may this destination be reached — and
//!   never resolves anything itself, which is what makes the rules testable
//!   offline;
//! - the proxy (added with the transport) is the only component that touches the
//!   network, and it asks the matcher rather than deciding.
//!
//! A sandbox never reaches the internet directly: the child only ever sees one
//! hole (a unix socket inside a network namespace, or a single loopback port
//! under seatbelt), and everything through that hole is decided here.
//!
//! The transport (proxy, bridge, per-backend wiring) is the other half and lands
//! with it; until then this module is exercised by its own tests, so the
//! dead-code allowance stays until the proxy calls into it.
#![allow(dead_code)]

pub mod bridge;
pub(crate) mod matcher;
pub(crate) mod pattern;
pub(crate) mod proxy;

use std::time::Duration;

/// How many tunnels the proxy keeps open at once, until a deployment says otherwise.
pub const DEFAULT_PROXY_MAX_CONNECTIONS: usize = 256;
/// How long a tunnel may sit without a byte moving, until a deployment says otherwise.
pub const DEFAULT_PROXY_IDLE_TIMEOUT: Duration = Duration::from_secs(60);
/// How long a tunnel may stay open at all, until a deployment says otherwise.
pub const DEFAULT_PROXY_MAX_LIFETIME: Duration = Duration::from_secs(30 * 60);

/// The bounds the enforcement proxy holds every live tunnel to.
///
/// These are plain values rather than `Option`s: the failures they prevent —
/// an unbounded set of concurrent tunnels, a tunnel that never ends, a client
/// that opens one and walks away — are not modes worth shipping, so a deployment
/// that wants a different number sets one instead of turning the bound off.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ProxyLimits {
    /// Tunnels and forwarded requests that may be live at once.
    ///
    /// A connection that cannot take a slot is refused at once (HTTP `503`,
    /// SOCKS5 general failure) rather than parked: a queue would turn a latency
    /// problem into a memory problem. Must be at least one; settings validation
    /// rejects zero rather than reading it as "unbounded".
    pub max_connections: usize,
    /// How long a tunnel may go without a byte moving in either direction.
    ///
    /// Seconds when written as settings (`net.proxy.idle_timeout_secs`). `0`
    /// disables idle detection, which a deployment that deliberately holds quiet
    /// long-lived tunnels can opt into; the lifetime cap still applies.
    pub idle_timeout: Duration,
    /// How long a tunnel may stay open at all, however busy it is.
    ///
    /// Seconds when written as settings (`net.proxy.max_lifetime_secs`). `0`
    /// disables the cap, which leaves a busy tunnel bounded only by the client
    /// ending it; the default exists so that this is not what a deployment gets
    /// by accident.
    pub max_lifetime: Duration,
}

impl Default for ProxyLimits {
    fn default() -> Self {
        Self {
            max_connections: DEFAULT_PROXY_MAX_CONNECTIONS,
            idle_timeout: DEFAULT_PROXY_IDLE_TIMEOUT,
            max_lifetime: DEFAULT_PROXY_MAX_LIFETIME,
        }
    }
}
