//! The remote client: a remote workspace presented as the local contract.

use std::sync::Arc;

use async_trait::async_trait;
use flowcraft_client::grpc::from_status;
use flowcraft_client::wire::Wire;
use flowcraft_error::{Error, Result};
use flowcraft_kernel::{EndpointProvider, Endpoints};
use flowcraft_proto::workspace::{
    AppendRequest, DeleteRequest, DescribeRequest, ExistsRequest, ListRequest, ReadRequest,
    RemoveAllRequest, RenameRequest, StatRequest, WriteRequest, workspace_client::WorkspaceClient,
};
use flowcraft_resource::Kind;
use flowcraft_resource::{BoxFuture, BuildCtx, Factory, Phase, Spec, require};
use serde::Deserialize;
use serde_json::Value;

use crate::contract::{CONTRACT, Capabilities, Entry, Handle, Stat, Workspace};
use crate::scope::Scope;

/// The implementation name.
pub const IMPL: &str = "remote";
/// The endpoint provider's contract.
pub const ENDPOINTS_CONTRACT: &str = flowcraft_kernel::handles::contracts::ENDPOINT_PROVIDER;

/// A workspace served by another process.
///
/// The channel is opened lazily on the first call: a build phase runs before
/// anything is started, so connecting at construction would race the endpoint's
/// listener.
pub struct RemoteWorkspace {
    wire: Wire,
}

impl std::fmt::Debug for RemoteWorkspace {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("RemoteWorkspace")
            .field("wire", &self.wire)
            .finish_non_exhaustive()
    }
}

impl RemoteWorkspace {
    /// Mint a grant and prepare a client. No I/O: the channel opens on first use.
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

    async fn client(
        &self,
    ) -> Result<WorkspaceClient<flowcraft_client::trace::TraceService<tonic::transport::Channel>>>
    {
        Ok(WorkspaceClient::new(self.wire.channel().await?))
    }
}

#[async_trait]
impl Workspace for RemoteWorkspace {
    async fn capabilities(&self) -> Result<Capabilities> {
        // No silent default: an unreachable endpoint must not look like a
        // provider with no guarantees.
        let mut client = self.client().await?;
        let request = self.wire.request(DescribeRequest {}).await?;
        let response = client.describe(request).await.map_err(from_status)?;
        let payload = response.into_inner();
        Ok(Capabilities {
            atomic_rename: payload.atomic_rename,
            read_after_write: payload.read_after_write,
            durable_on_write: payload.durable_on_write,
            distributed: payload.distributed,
        })
    }

    async fn read(&self, path: &str) -> Result<Vec<u8>> {
        let mut client = self.client().await?;
        let request = self
            .wire
            .request(ReadRequest {
                path: path.to_owned(),
            })
            .await?;
        let response = client.read(request).await.map_err(from_status)?;
        Ok(response.into_inner().data)
    }

    async fn write(&self, path: &str, data: &[u8]) -> Result<()> {
        let mut client = self.client().await?;
        let request = self
            .wire
            .request(WriteRequest {
                path: path.to_owned(),
                data: data.to_vec(),
            })
            .await?;
        client.write(request).await.map_err(from_status)?;
        Ok(())
    }

    async fn append(&self, path: &str, data: &[u8]) -> Result<()> {
        let mut client = self.client().await?;
        let request = self
            .wire
            .request(AppendRequest {
                path: path.to_owned(),
                data: data.to_vec(),
            })
            .await?;
        client.append(request).await.map_err(from_status)?;
        Ok(())
    }

    async fn rename(&self, from: &str, to: &str) -> Result<()> {
        let mut client = self.client().await?;
        let request = self
            .wire
            .request(RenameRequest {
                src: from.to_owned(),
                dst: to.to_owned(),
            })
            .await?;
        client.rename(request).await.map_err(from_status)?;
        Ok(())
    }

    async fn delete(&self, path: &str) -> Result<()> {
        let mut client = self.client().await?;
        let request = self
            .wire
            .request(DeleteRequest {
                path: path.to_owned(),
            })
            .await?;
        client.delete(request).await.map_err(from_status)?;
        Ok(())
    }

    async fn remove_all(&self, path: &str) -> Result<()> {
        let mut client = self.client().await?;
        let request = self
            .wire
            .request(RemoveAllRequest {
                path: path.to_owned(),
            })
            .await?;
        client.remove_all(request).await.map_err(from_status)?;
        Ok(())
    }

    async fn stat(&self, path: &str) -> Result<Stat> {
        let mut client = self.client().await?;
        let request = self
            .wire
            .request(StatRequest {
                path: path.to_owned(),
            })
            .await?;
        let response = client.stat(request).await.map_err(from_status)?;
        let payload = response.into_inner();
        Ok(Stat {
            name: payload.name,
            size: payload.size.max(0) as u64,
            mode: payload.mode,
            modified_unix_nanos: payload.mod_time_unix_nano,
            is_dir: payload.is_dir,
        })
    }

    async fn exists(&self, path: &str) -> Result<bool> {
        let mut client = self.client().await?;
        let request = self
            .wire
            .request(ExistsRequest {
                path: path.to_owned(),
            })
            .await?;
        let response = client.exists(request).await.map_err(from_status)?;
        Ok(response.into_inner().exists)
    }

    async fn list(&self, dir: &str) -> Result<Vec<Entry>> {
        let mut client = self.client().await?;
        let request = self
            .wire
            .request(ListRequest {
                dir: dir.to_owned(),
            })
            .await?;
        let response = client.list(request).await.map_err(from_status)?;
        Ok(response
            .into_inner()
            .entries
            .into_iter()
            .map(|entry| Entry {
                name: entry.name,
                is_dir: entry.is_dir,
                mode: entry.mode,
                size: entry.size.max(0) as u64,
                modified_unix_nanos: entry.mod_time_unix_nano,
            })
            .collect())
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
    // The scope is signed into the grant; validate it here so a typo fails the
    // plan instead of the first request.
    Scope::parse(&settings.scope)?;
    Ok(())
}

/// The `workspace.Workspace/remote` factory.
pub struct RemoteFactory;

impl Factory for RemoteFactory {
    type Output = Handle;

    fn spec(&self) -> Spec {
        let kind = Kind::new(crate::contract::KIND)
            .unwrap_or_else(|error| panic!("workspace kind is invalid: {error}"));
        let mut endpoints = require::<Endpoints>("endpoints");
        endpoints.contract = Some(ENDPOINTS_CONTRACT);
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
            let workspace = RemoteWorkspace::connect(
                endpoints.0.clone(),
                &settings.endpoint,
                &settings.tenant,
                scope,
            )
            .await?;
            Ok(Arc::new(Handle::new(Arc::new(workspace))))
        })
    }
}
