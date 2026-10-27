//! The assembled kernel.

use std::sync::Arc;
use std::time::{Duration, SystemTime};

use crate::error::{Error, Result};
use serde_json::Value;

use crate::kernel::catalog::{Endpoint, StaticCatalog};
use crate::kernel::config::{Config, parse_target};
use crate::kernel::grant::{Clock, Grant, GrantIssuer, GrantRequest, MemoryIssuer};
use crate::kernel::handles::Resolved;
use crate::kernel::host::EndpointHost;
use crate::kernel::secret_provider::{Chain, SecretProvider};
use crate::kernel::tls;

/// Configuration, catalog, grants, and listeners, assembled in a fixed order.
///
/// The kernel is code-first on purpose: it is not a resource graph, and no
/// deployment document can replace a component of it.
#[derive(Debug, Clone)]
pub struct Platform {
    config: Config,
    catalog: Arc<StaticCatalog>,
    issuer: Arc<MemoryIssuer>,
    host: Arc<EndpointHost>,
    secrets: Arc<dyn SecretProvider>,
    /// Set by the admin `drain` command: no new leases are minted.
    draining: Arc<std::sync::atomic::AtomicBool>,
}

impl Platform {
    /// Assemble and validate. Performs no I/O.
    pub fn new(config: Config) -> Result<Self> {
        config.validate()?;
        Self::from_validated(config, Arc::new(Chain::default()))
    }

    /// Assemble with a bootstrap secret provider (TLS material).
    pub fn with_secrets(config: Config, secrets: Arc<dyn SecretProvider>) -> Result<Self> {
        config.validate()?;
        Self::from_validated(config, secrets)
    }

    /// Assemble with an injected clock (tests).
    pub fn with_clock(config: Config, clock: Clock) -> Result<Self> {
        config.validate()?;
        let issuer =
            MemoryIssuer::new(Duration::from_secs(config.grants.ttl_secs)).with_clock(clock);
        Self::assemble(config, Arc::new(Chain::default()), issuer)
    }

    fn from_validated(config: Config, secrets: Arc<dyn SecretProvider>) -> Result<Self> {
        let issuer = MemoryIssuer::new(Duration::from_secs(config.grants.ttl_secs));
        Self::assemble(config, secrets, issuer)
    }

    fn assemble(
        config: Config,
        secrets: Arc<dyn SecretProvider>,
        issuer: MemoryIssuer,
    ) -> Result<Self> {
        let catalog = Arc::new(StaticCatalog::from_config(&config));
        Ok(Self {
            host: Arc::new(EndpointHost::new(catalog.clone()).with_secrets(secrets.clone())),
            config,
            catalog,
            issuer: Arc::new(issuer),
            secrets,
            draining: Arc::new(std::sync::atomic::AtomicBool::new(false)),
        })
    }

    /// The host configuration.
    #[must_use]
    pub fn config(&self) -> &Config {
        &self.config
    }

    /// The endpoint catalog.
    #[must_use]
    pub fn catalog(&self) -> Arc<StaticCatalog> {
        self.catalog.clone()
    }

    /// The grant issuer, injectable as an endpoint dependency.
    #[must_use]
    pub fn issuer(&self) -> Arc<MemoryIssuer> {
        self.issuer.clone()
    }

    /// The bootstrap secret provider: endpoint TLS material and a craft's
    /// pre-shared token resolve against the same source.
    #[must_use]
    pub fn secrets(&self) -> Arc<dyn SecretProvider> {
        self.secrets.clone()
    }

    /// The listener factory, injectable as an endpoint dependency.
    #[must_use]
    pub fn endpoint_host(&self) -> Arc<EndpointHost> {
        self.host.clone()
    }

    /// Resolve an endpoint by name.
    pub fn resolve(&self, name: &str) -> Result<Endpoint> {
        self.catalog.resolve(name)
    }

    /// Mint a grant for one capability at one endpoint.
    ///
    /// The scope is frozen into the token: an endpoint only ever checks that a
    /// request fits inside what was signed, never widens it.
    pub fn grant(
        &self,
        endpoint: &str,
        capability: &str,
        session: &str,
        tenant: &str,
        scope: Value,
    ) -> Result<Grant> {
        if self.draining.load(std::sync::atomic::Ordering::SeqCst) {
            return Err(Error::not_available(
                "platform is draining; no new grants are issued",
            ));
        }
        let descriptor = self.catalog.resolve(endpoint)?;
        if !descriptor.supports(capability) {
            return Err(Error::not_available(format!(
                "endpoint {endpoint:?} does not serve {capability:?}"
            )));
        }
        if !descriptor.tenant.is_empty() && descriptor.tenant != tenant {
            return Err(Error::forbidden(format!(
                "endpoint {endpoint:?} serves tenant {:?}, not {tenant:?}",
                descriptor.tenant
            )));
        }
        self.issuer.issue(&GrantRequest {
            session: session.to_owned(),
            tenant: tenant.to_owned(),
            capability: capability.to_owned(),
            endpoint: endpoint.to_owned(),
            scope,
            ttl: None,
        })
    }

    /// Validate a presented token.
    pub fn authenticate(&self, token: &str) -> Result<Grant> {
        self.issuer.validate(token)
    }

    /// Revoke every grant of a session. Called on session close, craft crash,
    /// and drain.
    pub fn revoke_session(&self, session: &str) -> Result<()> {
        self.issuer.revoke_session(session)
    }

    /// Stop issuing new grants. Existing leases stay valid until they expire or
    /// are revoked.
    pub fn drain(&self) {
        self.draining
            .store(true, std::sync::atomic::Ordering::SeqCst);
    }

    /// Whether the platform is draining.
    #[must_use]
    pub fn is_draining(&self) -> bool {
        self.draining.load(std::sync::atomic::Ordering::SeqCst)
    }

    /// Current time as the kernel sees it, for tests that inject a clock.
    #[must_use]
    pub fn now(&self) -> SystemTime {
        SystemTime::now()
    }

    /// The grants handle, for injection into endpoint resources.
    #[must_use]
    pub fn grants(&self) -> crate::kernel::handles::Grants {
        crate::kernel::handles::Grants(self.issuer.clone())
    }

    /// The endpoint provider handle, for injection into remote client
    /// resources.
    #[must_use]
    pub fn endpoints(&self) -> crate::kernel::handles::Endpoints {
        crate::kernel::handles::Endpoints(Arc::new(self.clone()))
    }

    /// The session every bootstrap-issued grant is bound to.
    ///
    /// A real deployment sets this when the platform opens a session; the
    /// standalone daemon uses one bootstrap session until `craft.Craft/v1`
    /// lands.
    #[must_use]
    pub fn session(&self) -> &'static str {
        "bootstrap"
    }
}

impl crate::kernel::handles::EndpointProvider for Platform {
    fn resolve(&self, endpoint: &str) -> Result<Resolved> {
        let descriptor = self.catalog.resolve(endpoint)?;
        let target = parse_target(&descriptor.target)?;
        let tls = match &descriptor.tls {
            Some(config) => tls::client(config, &*self.secrets)?,
            None => None,
        };
        Ok(Resolved { target, tls })
    }

    fn grant_scope(
        &self,
        endpoint: &str,
        capability: &str,
        tenant: &str,
        scope: Value,
    ) -> Result<Grant> {
        self.grant(endpoint, capability, self.session(), tenant, scope)
    }
}

impl GrantIssuer for Platform {
    fn issue(&self, request: &GrantRequest) -> Result<Grant> {
        self.issuer.issue(request)
    }

    fn validate(&self, token: &str) -> Result<Grant> {
        self.issuer.validate(token)
    }

    fn revoke(&self, id: &str) -> Result<()> {
        self.issuer.revoke(id)
    }

    fn revoke_session(&self, session: &str) -> Result<()> {
        self.issuer.revoke_session(session)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::kernel::config::{EndpointConfig, Mode, RemoteConfig};
    use std::collections::BTreeMap;

    fn config() -> Config {
        Config {
            remote: RemoteConfig {
                mode: Mode::Trusted,
                allow_targets: Vec::new(),
                endpoints: BTreeMap::from([(
                    "ws".to_owned(),
                    EndpointConfig {
                        target: "unix:///tmp/flowcraft-test/ws.sock".to_owned(),
                        tenant: "a".to_owned(),
                        capabilities: vec!["workspace.Workspace/v1".to_owned()],
                        tls: None,
                    },
                )]),
            },
            ..Config::default()
        }
    }

    #[test]
    fn grant_and_authenticate_round_trip() {
        let platform = Platform::new(config()).unwrap();
        let grant = platform
            .grant(
                "ws",
                "workspace.Workspace/v1",
                "s-1",
                "a",
                serde_json::json!({"root": "repos", "write": true}),
            )
            .unwrap();
        let recovered = platform.authenticate(&grant.token).unwrap();
        assert_eq!(recovered.session, "s-1");
        assert_eq!(recovered.scope["root"], "repos");

        let endpoint = platform.resolve("ws").unwrap();
        endpoint.authorize(&recovered).unwrap();
    }

    #[test]
    fn tenant_is_enforced_at_issue_time() {
        let platform = Platform::new(config()).unwrap();
        let error = platform
            .grant(
                "ws",
                "workspace.Workspace/v1",
                "s-1",
                "b",
                serde_json::json!({}),
            )
            .unwrap_err();
        assert_eq!(error.kind(), crate::error::ErrorKind::Forbidden);
    }

    #[test]
    fn unsupported_capability_is_not_available() {
        let platform = Platform::new(config()).unwrap();
        let error = platform
            .grant("ws", "sandbox.Runner/v1", "s-1", "a", serde_json::json!({}))
            .unwrap_err();
        assert_eq!(error.kind(), crate::error::ErrorKind::NotAvailable);
    }

    #[test]
    fn draining_stops_new_grants_but_keeps_existing_leases() {
        let platform = Platform::new(config()).unwrap();
        let grant = platform
            .grant(
                "ws",
                "workspace.Workspace/v1",
                "s-1",
                "a",
                serde_json::json!({}),
            )
            .unwrap();
        platform.drain();
        assert!(platform.is_draining());
        // Existing leases stay valid until they expire or are revoked.
        assert_eq!(platform.authenticate(&grant.token).unwrap().session, "s-1");
        let error = platform
            .grant(
                "ws",
                "workspace.Workspace/v1",
                "s-2",
                "a",
                serde_json::json!({}),
            )
            .unwrap_err();
        assert_eq!(error.kind(), crate::error::ErrorKind::NotAvailable);
    }

    #[test]
    fn session_revocation_invalidates_every_grant() {
        let platform = Platform::new(config()).unwrap();
        let first = platform
            .grant(
                "ws",
                "workspace.Workspace/v1",
                "s-1",
                "a",
                serde_json::json!({}),
            )
            .unwrap();
        let second = platform
            .grant(
                "ws",
                "workspace.Workspace/v1",
                "s-1",
                "a",
                serde_json::json!({}),
            )
            .unwrap();
        platform.revoke_session("s-1").unwrap();
        assert!(platform.authenticate(&first.token).is_err());
        assert!(platform.authenticate(&second.token).is_err());
    }
}
