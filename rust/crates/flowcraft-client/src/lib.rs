//! Client-side plumbing shared by every capability crate.
//!
//! Nothing here is a server concern: [`wire::Wire`] mints and renews the grant a
//! remote client presents, [`grpc`] maps the error taxonomy to statuses and
//! checks bearer grants, and [`craft`] speaks the platform-to-craft ABI.
//!
//! Keeping this out of `flowcraft-kernel` means a remote capability crate does
//! not pull in the control plane's config, craft lifecycle, or TLS policy.

#![cfg_attr(test, allow(clippy::unwrap_used, clippy::expect_used))]

pub mod craft;
pub mod grpc;
pub mod trace;
pub mod wire;

pub use craft::CraftClient;
pub use wire::Wire;
