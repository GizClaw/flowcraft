#![cfg_attr(test, allow(clippy::unwrap_used, clippy::expect_used))]

//! The secret capability.
//!
//! One contract ([`contract::Store`]) with four providers (`memory`, `env`,
//! `file`, `keychain`), an endpoint that serves a provider over gRPC, and a
//! remote client that presents it as the same contract. A provider that can be
//! written to also exposes [`contract::Mutable`], and [`keychain::Manager`] is
//! the app-side handle over the encrypted store.
//!
//! This is the *application* secret store: business credentials resolved with
//! `${secret:...}`. Bootstrap material (endpoint TLS keys) belongs to the
//! kernel's provider and never travels this wire.

pub mod codes;
pub mod contract;
pub mod env;
pub mod file;
pub mod keychain;
pub mod memory;

// The endpoint server, the remote client, and the grant-scope checks they share.
#[cfg(feature = "wire")]
pub mod endpoint;
#[cfg(feature = "wire")]
pub mod remote;
#[cfg(feature = "wire")]
pub mod scope;

pub use contract::{CONTRACT, Handle, KIND, Mutable, Store};
pub use env::EnvStore;
pub use keychain::{Keychain, Manager};
pub use memory::MemoryStore;
#[cfg(feature = "wire")]
pub use remote::RemoteStore;
