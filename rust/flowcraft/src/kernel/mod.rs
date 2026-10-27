//! The code-first control plane.
//!
//! The kernel owns configuration, endpoint resolution, grants, and listener
//! creation. It deliberately does **not** build itself from a resource
//! document: its components are process-scoped with a fixed startup order and a
//! security boundary, so a tenant document must not be able to replace the
//! catalog or widen the target policy.
//!
//! Local, pluggable providers live in the composition graph instead
//! (`flowcraft-resource`); remote capabilities are endpoints reached through
//! grants minted here.
//!
//! # Transport
//!
//! Endpoints are unix-domain sockets by default: owner-only, and recovered from
//! a stale socket file on rebind. A `dns://` target is served over TCP with
//! TLS/mTLS instead, and one whose `tls` block is missing is rejected at startup
//! rather than served in the clear — there is no plaintext fallback for a network
//! address.
//!
//! A `tls` block holds `${secret:...}` references only. The bytes come from the
//! host's bootstrap [`secret_provider`], so no certificate or private key ever
//! appears in a document. Replacing those files and reloading (`SIGHUP`, or the
//! `bootstrap.dir` watcher) picks the new material up, because an endpoint reads
//! it when it binds.

#![cfg_attr(test, allow(clippy::unwrap_used, clippy::expect_used))]
pub mod catalog;
pub mod config;
pub mod grant;
pub mod handles;
pub mod host;
pub mod identity;
pub mod net;
pub mod platform;
pub mod secret_provider;
pub mod tls;
pub mod transport;

pub use catalog::{Endpoint, StaticCatalog};
pub use config::{
    BootstrapConfig, Config, CraftConfig, CraftGrant, CraftStdio, EndpointConfig, GrantsConfig,
    ListenConfig, LogConfig, Mode, RemoteConfig, Target, TlsConfig,
};
pub use grant::{Grant, GrantIssuer, GrantRequest, MemoryIssuer};
pub use handles::{EndpointProvider, Endpoints, Grants, Resolved};
pub use host::{BoundServer, EndpointHost, Serving, TraceLayer};
pub use identity::{MachineIdentity, Scope};
pub use platform::Platform;
pub use secret_provider::{Chain, EnvProvider, FileProvider, SecretProvider};
