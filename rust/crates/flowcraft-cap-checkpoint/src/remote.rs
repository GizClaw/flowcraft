//! The remote client: a remote checkpoint store presented as the local
//! contract.

use std::sync::Arc;

use async_trait::async_trait;
use flowcraft_client::grpc::from_status;
use flowcraft_client::wire::Wire;
use flowcraft_error::{Error, Result};
use flowcraft_kernel::{EndpointProvider, Endpoints};
use flowcraft_proto::checkpoint::{
    DeleteRequest, ListRequest, LoadRequest, SaveRequest, checkpoint_client::CheckpointClient,
};
use flowcraft_resource::Kind;
use flowcraft_resource::{BoxFuture, BuildCtx, Factory, Phase, Spec, require};
use serde::Deserialize;
use serde_json::Value;

use crate::contract::{CONTRACT, Handle, KIND, Store, extract_exec_id};
use crate::scope::Scope;

/// The implementation name.
pub const IMPL: &str = "remote";

/// A checkpoint store served by another process.
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
    /// Mint a grant and prepare a client. No I/O until the first call.
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
    async fn save(&self, exec_id: &str, checkpoint: &[u8]) -> Result<()> {
        // The wire carries the id inside the payload. Refuse a mismatch here so
        // the error names the caller, not the remote server.
        let carried = extract_exec_id(checkpoint)?;
        if carried != exec_id {
            return Err(Error::validation(format!(
                "checkpoint payload is for {carried:?}, but save was called for {exec_id:?}"
            )));
        }
        let mut client = CheckpointClient::new(self.wire.channel().await?);
        let request = self
            .wire
            .request(SaveRequest {
                checkpoint: checkpoint.to_vec(),
            })
            .await?;
        client.save(request).await.map_err(from_status)?;
        Ok(())
    }

    async fn load(&self, exec_id: &str) -> Result<Option<Vec<u8>>> {
        let mut client = CheckpointClient::new(self.wire.channel().await?);
        let request = self
            .wire
            .request(LoadRequest {
                exec_id: exec_id.to_owned(),
            })
            .await?;
        let response = client.load(request).await.map_err(from_status)?;
        let payload = response.into_inner();
        if payload.found {
            Ok(Some(payload.checkpoint))
        } else {
            Ok(None)
        }
    }

    async fn list(&self) -> Result<Vec<String>> {
        let mut client = CheckpointClient::new(self.wire.channel().await?);
        let request = self.wire.request(ListRequest {}).await?;
        let response = client.list(request).await.map_err(from_status)?;
        Ok(response.into_inner().exec_ids)
    }

    async fn delete(&self, exec_id: &str) -> Result<()> {
        let mut client = CheckpointClient::new(self.wire.channel().await?);
        let request = self
            .wire
            .request(DeleteRequest {
                exec_id: exec_id.to_owned(),
            })
            .await?;
        client.delete(request).await.map_err(from_status)?;
        Ok(())
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
    let settings: Settings = flowcraft_resource::settings(value)?;
    if settings.endpoint.trim().is_empty() {
        return Err(Error::validation("settings.endpoint is required"));
    }
    Scope::parse(&settings.scope)?;
    Ok(())
}

/// The `checkpoint.Store/remote` factory.
pub struct RemoteFactory;

impl Factory for RemoteFactory {
    type Output = Handle;

    fn spec(&self) -> Spec {
        let kind = Kind::new(KIND).unwrap_or_else(|error| panic!("{KIND} is invalid: {error}"));
        let mut endpoints = require::<Endpoints>("endpoints");
        endpoints.contract = Some(flowcraft_kernel::handles::contracts::ENDPOINT_PROVIDER);
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
            let scope = if settings.scope.is_null() {
                serde_json::json!({})
            } else {
                settings.scope
            };
            let store = RemoteStore::connect(
                endpoints.0.clone(),
                &settings.endpoint,
                &settings.tenant,
                scope,
            )
            .await?;
            Ok(Arc::new(Handle::new(Arc::new(store))))
        })
    }
}
