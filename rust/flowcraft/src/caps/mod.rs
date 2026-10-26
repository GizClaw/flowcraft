//! The built-in capability implementations.
//!
//! Each module is the whole capability: the contract and its local providers,
//! the endpoint server, and the remote client that presents the same contract
//! over the wire. The endpoint and the remote client are only compiled with the
//! `wire` feature, because they are the part that needs the control plane: a
//! host that embeds a provider does not link a listener or a TLS policy.
//!
//! Every capability registers its document `Kind` from its own module, so a
//! deployment assembled from `resource::Registry` finds them without a central
//! list here.

#[cfg(feature = "workspace")]
pub mod workspace;

#[cfg(feature = "secret")]
pub mod secret;

#[cfg(feature = "sandbox")]
pub mod sandbox;

#[cfg(feature = "tool")]
pub mod tool;
