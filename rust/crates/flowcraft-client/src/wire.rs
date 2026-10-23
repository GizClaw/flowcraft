//! The client side of a capability wire.
//!
//! A remote client resource and its endpoint server share one grant and one
//! channel lifecycle. [`Wire`] owns both: it mints the grant at construction
//! (no I/O), opens the channel on first use — build runs before start, so
//! connecting eagerly would race the listener — renews the lease before it
//! expires, and stamps every request with the current bearer token.

use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use flowcraft_error::{Error, Result};
use serde_json::Value;
use tonic::metadata::MetadataValue;
use tonic::transport::Channel;
use tower::Layer as _;

use flowcraft_kernel::grant::Grant;
use flowcraft_kernel::handles::EndpointProvider;

/// Renew once the lease has less than this left.
const REFRESH_MARGIN: Duration = Duration::from_secs(60);

/// A grant plus a lazily opened channel to one capability endpoint.
pub struct Wire {
    provider: Arc<dyn EndpointProvider>,
    endpoint: String,
    capability: String,
    tenant: String,
    scope: Value,
    grant: tokio::sync::RwLock<Grant>,
    refresh_gate: tokio::sync::Mutex<()>,
    channel: tokio::sync::Mutex<Option<Channel>>,
}

impl std::fmt::Debug for Wire {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Wire")
            .field("endpoint", &self.endpoint)
            .field("capability", &self.capability)
            .finish_non_exhaustive()
    }
}

impl Wire {
    /// Mint a grant for `capability` and prepare a channel. Performs no I/O.
    pub async fn open(
        provider: Arc<dyn EndpointProvider>,
        endpoint: &str,
        capability: &str,
        tenant: &str,
        scope: Value,
    ) -> Result<Self> {
        let grant = provider.grant_scope(endpoint, capability, tenant, scope.clone())?;
        Ok(Self {
            provider,
            endpoint: endpoint.to_owned(),
            capability: capability.to_owned(),
            tenant: tenant.to_owned(),
            scope,
            grant: tokio::sync::RwLock::new(grant),
            refresh_gate: tokio::sync::Mutex::new(()),
            channel: tokio::sync::Mutex::new(None),
        })
    }

    /// The grant in force.
    pub async fn grant(&self) -> Grant {
        self.grant.read().await.clone()
    }

    /// Renew the lease when it is close to expiring.
    ///
    /// A daemon outlives its grants: without this a long-running deployment
    /// would start failing every capability call once the TTL elapses.
    pub async fn refresh_if_needed(&self) -> Result<()> {
        if self.remaining() > REFRESH_MARGIN {
            return Ok(());
        }
        let _gate = self.refresh_gate.lock().await;
        // Another caller may have renewed while we waited for the gate.
        if self.remaining() > REFRESH_MARGIN {
            return Ok(());
        }
        let fresh = self.provider.grant_scope(
            &self.endpoint,
            &self.capability,
            &self.tenant,
            self.scope.clone(),
        )?;
        *self.grant.write().await = fresh;
        Ok(())
    }

    fn remaining(&self) -> Duration {
        // `try_read` is enough: a renewal in flight means somebody else is
        // already solving this, and the caller should not block on it.
        let now = now_secs();
        let expires = self
            .grant
            .try_read()
            .map(|grant| grant.expires_at)
            .unwrap_or(0);
        Duration::from_secs(expires.saturating_sub(now))
    }

    /// The channel, connected on first use and wrapped in a client span.
    ///
    /// Callers build their generated client from this, so every call they make
    /// is inside `rpc` — which is what carries the trace to the endpoint.
    pub async fn channel(&self) -> Result<crate::trace::TraceService<Channel>> {
        let mut guard = self.channel.lock().await;
        if let Some(channel) = guard.as_ref() {
            return Ok(crate::trace::TraceLayer.layer(channel.clone()));
        }
        let resolved = self.provider.resolve(&self.endpoint)?;
        let channel = flowcraft_kernel::transport::connect(&resolved.target, resolved.tls).await?;
        *guard = Some(channel.clone());
        Ok(crate::trace::TraceLayer.layer(channel))
    }

    /// Wrap a payload in a request carrying a live grant.
    pub async fn request<T>(&self, payload: T) -> Result<tonic::Request<T>> {
        self.refresh_if_needed().await?;
        let token = self.grant.read().await.token.clone();
        let mut request = tonic::Request::new(payload);
        let value = MetadataValue::try_from(format!("Bearer {token}")).map_err(|error| {
            Error::internal(format!("grant token is not valid metadata: {error}"))
        })?;
        request.metadata_mut().insert("authorization", value);
        Ok(request)
    }
}

fn now_secs() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|duration| duration.as_secs())
        .unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::grpc::bearer;
    use flowcraft_kernel::config::{Config, EndpointConfig, Mode, RemoteConfig};
    use flowcraft_kernel::platform::Platform;
    use std::collections::BTreeMap;

    fn platform(ttl_secs: u64) -> Platform {
        let config = Config {
            remote: RemoteConfig {
                mode: Mode::Trusted,
                allow_targets: Vec::new(),
                endpoints: BTreeMap::from([(
                    "ws".to_owned(),
                    EndpointConfig {
                        target: "unix:///tmp/flowcraft-wire/ws.sock".to_owned(),
                        tenant: String::new(),
                        capabilities: vec!["workspace.Workspace/v1".to_owned()],
                        tls: None,
                    },
                )]),
            },
            grants: flowcraft_kernel::config::GrantsConfig { ttl_secs },
            ..Config::default()
        };
        Platform::new(config).expect("platform")
    }

    #[tokio::test]
    async fn a_grant_is_minted_without_connecting() {
        let platform = platform(900);
        let wire = Wire::open(
            platform.endpoints().0,
            "ws",
            "workspace.Workspace/v1",
            "",
            serde_json::json!({}),
        )
        .await
        .expect("wire");
        let grant = wire.grant().await;
        assert_eq!(grant.endpoint, "ws");
        assert!(grant.token.contains('.'));
    }

    #[tokio::test]
    async fn a_remote_capability_is_refused_before_dialling() {
        let platform = platform(900);
        let error = Wire::open(
            platform.endpoints().0,
            "ws",
            "sandbox.Runner/v1",
            "",
            serde_json::json!({}),
        )
        .await
        .expect_err("capability");
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::NotAvailable);
    }

    #[tokio::test]
    async fn requests_carry_the_bearer() {
        let platform = platform(900);
        let wire = Wire::open(
            platform.endpoints().0,
            "ws",
            "workspace.Workspace/v1",
            "",
            serde_json::json!({}),
        )
        .await
        .expect("wire");
        let request = wire.request(()).await.expect("request");
        assert_eq!(
            bearer(&request).expect("bearer"),
            wire.grant().await.token.as_str()
        );
    }

    #[tokio::test]
    async fn an_expiring_grant_is_renewed_in_place() {
        // A one-second lease leaves less than the refresh margin immediately.
        let platform = platform(1);
        let wire = Wire::open(
            platform.endpoints().0,
            "ws",
            "workspace.Workspace/v1",
            "",
            serde_json::json!({}),
        )
        .await
        .expect("wire");
        let before = wire.grant().await;
        tokio::time::sleep(Duration::from_millis(1100)).await;
        wire.refresh_if_needed().await.expect("refresh");
        let after = wire.grant().await;
        assert_ne!(before.token, after.token);
        assert!(after.expires_at > before.expires_at);
    }
}
