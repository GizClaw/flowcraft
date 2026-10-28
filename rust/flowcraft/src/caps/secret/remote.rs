//! The remote client: a remote secret store presented as the local contract.

use std::sync::Arc;

use crate::client::grpc::from_status;
use crate::client::wire::Wire;
use crate::error::{Error, Result};
use crate::kernel::{EndpointProvider, Endpoints};
use crate::proto::secret::{LookupRequest, secret_client::SecretClient};
use crate::resource::Kind;
use crate::resource::{BoxFuture, BuildCtx, Factory, Phase, Spec, require};
use async_trait::async_trait;
use serde::Deserialize;
use serde_json::Value;

use crate::caps::secret::contract::{CONTRACT, Handle, KIND, Store};
use crate::caps::secret::scope::Scope;

/// The implementation name.
pub const IMPL: &str = "remote";

/// A secret store served by another process.
pub struct RemoteStore {
    wire: Wire,
}

impl std::fmt::Debug for RemoteStore {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("RemoteStore")
            .field("wire", &self.wire)
            .finish_non_exhaustive()
    }
}

impl RemoteStore {
    /// Mint a grant and prepare a client. No I/O until the first lookup.
    pub async fn connect(
        provider: Arc<dyn EndpointProvider>,
        endpoint: &str,
        tenant: &str,
        scope: Value,
    ) -> Result<Self> {
        Ok(Self {
            wire: Wire::open(provider, endpoint, CONTRACT, tenant, scope).await?,
        })
    }
}

#[async_trait]
impl Store for RemoteStore {
    async fn lookup(&self, name: &str) -> Result<Option<String>> {
        let mut client = SecretClient::new(self.wire.channel().await?);
        let request = self
            .wire
            .request(LookupRequest {
                name: name.to_owned(),
            })
            .await?;
        let response = client.lookup(request).await.map_err(from_status)?;
        let payload = response.into_inner();
        if payload.found {
            Ok(Some(payload.value))
        } else {
            Ok(None)
        }
    }
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct Settings {
    endpoint: String,
    #[serde(default)]
    tenant: String,
    #[serde(default)]
    scope: Value,
}

fn validate(value: &Value) -> Result<()> {
    let settings: Settings = crate::resource::settings(value)?;
    if settings.endpoint.trim().is_empty() {
        return Err(Error::validation("settings.endpoint is required"));
    }
    Scope::parse(&settings.scope)?;
    Ok(())
}

/// The `secret.Store/remote` factory.
pub struct RemoteFactory;

impl Factory for RemoteFactory {
    type Output = Handle;

    fn spec(&self) -> Spec {
        let kind = Kind::new(KIND).unwrap_or_else(|error| panic!("{KIND} is invalid: {error}"));
        let mut endpoints = require::<Endpoints>("endpoints");
        endpoints.contract = Some(crate::kernel::handles::contracts::ENDPOINT_PROVIDER);
        Spec {
            contract: Some(CONTRACT),
            imp: Some(IMPL),
            deps: vec![endpoints],
            phase: Phase::Main,
            validate_settings: Some(validate),
            ..Spec::new(kind)
        }
    }

    fn build<'a>(&'a self, ctx: BuildCtx<'a>) -> BoxFuture<'a, Result<Arc<Self::Output>>> {
        Box::pin(async move {
            let settings: Settings = ctx.settings()?;
            let endpoints = ctx.deps.get::<Endpoints>("endpoints")?;
            let scope = Scope::parse(&settings.scope)?;
            let store = RemoteStore::connect(
                endpoints.0.clone(),
                &settings.endpoint,
                &settings.tenant,
                // Sign both spellings, so an endpoint running the other
                // implementation reads the same restriction instead of ignoring
                // a scope it does not know.
                scope.wire(),
            )
            .await?;
            Ok(Arc::new(Handle::new(Arc::new(store))))
        })
    }
}
