//! Kernel-owned handles that capabilities depend on.
//!
//! A resource requirement names a concrete type, so a trait object is wrapped
//! in a newtype handle. That keeps one stable requirement type per family while
//! still allowing the implementation behind it to vary (a real platform, a
//! test double, a future remote issuer).

use std::sync::Arc;

use crate::error::Result;
use crate::resource::Resource;
use serde_json::Value as Json;

use crate::kernel::config::Target;
use crate::kernel::grant::{Grant, GrantIssuer};

/// Contract identities shared by every capability endpoint.
///
/// They describe the *mechanism* (a listener factory, a grant issuer, a remote
/// endpoint resolver), not a capability, so every capability reuses them.
pub mod contracts {
    /// The host listener factory injected into endpoint resources.
    pub const ENDPOINT_HOST: &str = "capability.EndpointHost/v1";
    /// The grant issuer injected into endpoint resources.
    pub const GRANTS: &str = "capability.Grants/v1";
    /// The platform-injected endpoint resolver for remote client resources.
    pub const ENDPOINT_PROVIDER: &str = "capability.EndpointProvider/v1";
}

/// The platform's grant issuer, as a dependency.
#[derive(Clone)]
pub struct Grants(pub Arc<dyn GrantIssuer>);

impl Resource for Grants {}

impl std::fmt::Debug for Grants {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("Grants")
    }
}

/// A resolved endpoint target plus the TLS material to reach it.
#[derive(Clone)]
pub struct Resolved {
    /// Where it lives.
    pub target: Target,
    /// Client TLS, already built from bootstrap secrets.
    pub tls: Option<tonic::transport::ClientTlsConfig>,
}

impl std::fmt::Debug for Resolved {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Resolved")
            .field("target", &self.target)
            .field("tls", &self.tls.is_some())
            .finish()
    }
}

/// Host-side resolution of endpoint targets and grants, as a dependency of
/// remote client resources.
pub trait EndpointProvider: Send + Sync {
    /// Resolve an endpoint to a target and its client TLS material.
    fn resolve(&self, endpoint: &str) -> Result<Resolved>;

    /// Mint a grant for one capability at one endpoint.
    fn grant_scope(
        &self,
        endpoint: &str,
        capability: &str,
        tenant: &str,
        scope: Json,
    ) -> Result<Grant>;
}

/// The provider, as a dependency.
#[derive(Clone)]
pub struct Endpoints(pub Arc<dyn EndpointProvider>);

impl Resource for Endpoints {}

impl std::fmt::Debug for Endpoints {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("Endpoints")
    }
}
