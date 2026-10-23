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
