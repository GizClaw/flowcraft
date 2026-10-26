//! The endpoint server: a provider exposed over gRPC.

use std::sync::{Arc, Mutex};

use crate::client::grpc::{authenticate, grant_of, status};
use crate::error::{Error, Result};
use crate::kernel::{Endpoint as EndpointDescriptor, EndpointHost, Grants};
use crate::proto::workspace::{
    AppendRequest, AppendResponse, DeleteRequest, DeleteResponse, DescribeRequest,
    DescribeResponse, Entry as WireEntry, ExistsRequest, ExistsResponse, ListRequest, ListResponse,
    ReadLimitedRequest, ReadLimitedResponse, ReadRequest, ReadResponse, RemoveAllRequest,
    RemoveAllResponse, RenameRequest, RenameResponse, StatRequest, StatResponse, WriteRequest,
    WriteResponse, workspace_server,
};
use crate::resource::Kind;
use crate::resource::{BoxFuture, BuildCtx, Factory, Phase, Resource, Spec, require};
use serde::Deserialize;
use serde_json::Value;
use tonic::service::Interceptor;
use tonic::{Request, Response, Status};

use crate::caps::workspace::contract::{CONTRACT, Handle};
use crate::caps::workspace::scope::Scope;

/// The registry kind.
pub const KIND: &str = "workspace.Endpoint";
/// The implementation name.
pub const IMPL: &str = "grpc";
/// The capability this endpoint serves by default.
pub const CAPABILITY: &str = "workspace.Workspace/v1";

/// The resource's own contract identity.
pub const CONTRACT_ENDPOINT: &str = "workspace.Endpoint/v1";
/// The listener factory's contract.
pub const HOST_CONTRACT: &str = crate::kernel::handles::contracts::ENDPOINT_HOST;
/// The grant issuer's contract.
pub const GRANTS_CONTRACT: &str = crate::kernel::handles::contracts::GRANTS;

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct Settings {
    endpoint: String,
    #[serde(default)]
    capability: Option<String>,
}

fn validate(value: &Value) -> Result<()> {
    let settings: Settings = crate::resource::settings(value)?;
    if settings.endpoint.trim().is_empty() {
        return Err(Error::validation("settings.endpoint is required"));
    }
    Ok(())
}

/// A resource that serves a workspace on the endpoint named in its settings.
pub struct Endpoint {
    workspace: Handle,
    host: EndpointHost,
    grants: Grants,
    descriptor: EndpointDescriptor,
    capability: String,
    serving: Mutex<Option<crate::kernel::Serving>>,
}

impl std::fmt::Debug for Endpoint {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("workspace.Endpoint")
            .field("endpoint", &self.descriptor.name)
            .field("capability", &self.capability)
            .finish_non_exhaustive()
    }
}

impl Resource for Endpoint {
    fn start(self: Arc<Self>) -> BoxFuture<'static, Result<()>> {
        Box::pin(async move {
            let (mut server, listener) = self.host.bind(&self.descriptor.name)?;
            let service = Service {
                workspace: self.workspace.clone(),
                descriptor: self.descriptor.clone(),
                capability: self.capability.clone(),
            };
            let interceptor = GrantInterceptor {
                grants: self.grants.clone(),
                descriptor: self.descriptor.clone(),
                capability: self.capability.clone(),
            };
            let router = server.add_service(workspace_server::WorkspaceServer::with_interceptor(
                service,
                interceptor,
            ));
            let mut serving = self
                .serving
                .lock()
                .map_err(|_| Error::internal("endpoint lock poisoned"))?;
            *serving = Some(crate::kernel::Serving::spawn(router, listener));
            Ok(())
        })
    }

    fn shutdown(self: Arc<Self>) -> BoxFuture<'static, Result<()>> {
        Box::pin(async move {
            let handle = self
                .serving
                .lock()
                .map_err(|_| Error::internal("endpoint lock poisoned"))?
                .take();
            if let Some(serving) = handle {
                serving.shutdown().await?;
            }
            Ok(())
        })
    }
}

/// The `workspace.Endpoint/grpc` factory.
pub struct EndpointFactory;

impl Factory for EndpointFactory {
    type Output = Endpoint;

    fn spec(&self) -> Spec {
        let kind = Kind::new(KIND).unwrap_or_else(|error| panic!("{KIND} is invalid: {error}"));
        let mut workspace = require::<Handle>("workspace");
        workspace.contract = Some(CONTRACT);
        let mut host = require::<EndpointHost>("host");
        host.contract = Some(HOST_CONTRACT);
        let mut grants = require::<Grants>("grants");
        grants.contract = Some(GRANTS_CONTRACT);
        Spec {
            contract: Some(CONTRACT_ENDPOINT),
            imp: Some(IMPL),
            deps: vec![workspace, host, grants],
            phase: Phase::Main,
            validate_settings: Some(validate),
            ..Spec::new(kind)
        }
    }

    fn build<'a>(&'a self, ctx: BuildCtx<'a>) -> BoxFuture<'a, Result<Arc<Self::Output>>> {
        Box::pin(async move {
            let settings: Settings = ctx.settings()?;
            let host = ctx.deps.get::<EndpointHost>("host")?;
            let grants = ctx.deps.get::<Grants>("grants")?;
            let workspace = ctx.deps.get::<Handle>("workspace")?;

            let descriptor = host.catalog().resolve(&settings.endpoint)?;
            let capability = settings.capability.unwrap_or_else(|| CONTRACT.to_owned());
            if !descriptor.supports(&capability) {
                return Err(Error::not_available(format!(
                    "endpoint {:?} does not serve {capability:?}",
                    descriptor.name
                )));
            }

            Ok(Arc::new(Endpoint {
                workspace: Handle::new(workspace.0.clone()),
                host: (*host).clone(),
                grants: (*grants).clone(),
                descriptor,
                capability,
                serving: Mutex::new(None),
            }))
        })
    }
}

#[derive(Clone)]
struct GrantInterceptor {
    grants: Grants,
    descriptor: EndpointDescriptor,
    capability: String,
}

impl Interceptor for GrantInterceptor {
    fn call(&mut self, mut request: Request<()>) -> std::result::Result<Request<()>, Status> {
        let grant = authenticate(&request, &self.grants, &self.descriptor, &self.capability)?;
        request.extensions_mut().insert(grant);
        Ok(request)
    }
}

struct Service {
    workspace: Handle,
    descriptor: EndpointDescriptor,
    capability: String,
}

impl Service {
    fn authorize<T>(
        &self,
        request: &Request<T>,
        path: &str,
        write: bool,
    ) -> std::result::Result<(), Status> {
        let grant = grant_of(request)?;
        self.descriptor.authorize(&grant).map_err(status)?;
        if grant.capability != self.capability {
            return Err(Status::permission_denied("grant capability mismatch"));
        }
        let scope = Scope::parse(&grant.scope)
            .map_err(|_| Status::permission_denied("grant scope is invalid"))?;
        scope.allows(path, write).map_err(status)
    }
}

#[tonic::async_trait]
impl workspace_server::Workspace for Service {
    async fn describe(
        &self,
        _request: Request<DescribeRequest>,
    ) -> std::result::Result<Response<DescribeResponse>, Status> {
        let capabilities = self.workspace.capabilities().await.map_err(status)?;
        Ok(Response::new(DescribeResponse {
            atomic_rename: capabilities.atomic_rename,
            read_after_write: capabilities.read_after_write,
            durable_on_write: capabilities.durable_on_write,
            distributed: capabilities.distributed,
        }))
    }

    async fn read(
        &self,
        request: Request<ReadRequest>,
    ) -> std::result::Result<Response<ReadResponse>, Status> {
        let path = request.get_ref().path.clone();
        self.authorize(&request, &path, false)?;
        let data = self.workspace.read(&path).await.map_err(status)?;
        Ok(Response::new(ReadResponse { data }))
    }

    async fn read_limited(
        &self,
        request: Request<ReadLimitedRequest>,
    ) -> std::result::Result<Response<ReadLimitedResponse>, Status> {
        let (path, max_bytes) = {
            let payload = request.get_ref();
            (payload.path.clone(), payload.max_bytes)
        };
        self.authorize(&request, &path, false)?;
        let data = self
            .workspace
            .read_limited(&path, max_bytes)
            .await
            .map_err(status)?;
        Ok(Response::new(ReadLimitedResponse { data }))
    }

    async fn write(
        &self,
        request: Request<WriteRequest>,
    ) -> std::result::Result<Response<WriteResponse>, Status> {
        let (path, data) = {
            let payload = request.get_ref();
            (payload.path.clone(), payload.data.clone())
        };
        self.authorize(&request, &path, true)?;
        self.workspace.write(&path, &data).await.map_err(status)?;
        Ok(Response::new(WriteResponse {}))
    }

    async fn append(
        &self,
        request: Request<AppendRequest>,
    ) -> std::result::Result<Response<AppendResponse>, Status> {
        let (path, data) = {
            let payload = request.get_ref();
            (payload.path.clone(), payload.data.clone())
        };
        self.authorize(&request, &path, true)?;
        self.workspace.append(&path, &data).await.map_err(status)?;
        Ok(Response::new(AppendResponse {}))
    }

    async fn rename(
        &self,
        request: Request<RenameRequest>,
    ) -> std::result::Result<Response<RenameResponse>, Status> {
        let (from, to) = {
            let payload = request.get_ref();
            (payload.src.clone(), payload.dst.clone())
        };
        self.authorize(&request, &from, true)?;
        self.authorize(&request, &to, true)?;
        self.workspace.rename(&from, &to).await.map_err(status)?;
        Ok(Response::new(RenameResponse {}))
    }

    async fn delete(
        &self,
        request: Request<DeleteRequest>,
    ) -> std::result::Result<Response<DeleteResponse>, Status> {
        let path = request.get_ref().path.clone();
        self.authorize(&request, &path, true)?;
        self.workspace.delete(&path).await.map_err(status)?;
        Ok(Response::new(DeleteResponse {}))
    }

    async fn remove_all(
        &self,
        request: Request<RemoveAllRequest>,
    ) -> std::result::Result<Response<RemoveAllResponse>, Status> {
        let path = request.get_ref().path.clone();
        self.authorize(&request, &path, true)?;
        self.workspace.remove_all(&path).await.map_err(status)?;
        Ok(Response::new(RemoveAllResponse {}))
    }

    async fn stat(
        &self,
        request: Request<StatRequest>,
    ) -> std::result::Result<Response<StatResponse>, Status> {
        let path = request.get_ref().path.clone();
        self.authorize(&request, &path, false)?;
        let stat = self.workspace.stat(&path).await.map_err(status)?;
        Ok(Response::new(StatResponse {
            name: stat.name,
            size: stat.size as i64,
            mode: stat.mode,
            mod_time_unix_nano: stat.modified_unix_nanos,
            is_dir: stat.is_dir,
        }))
    }

    async fn exists(
        &self,
        request: Request<ExistsRequest>,
    ) -> std::result::Result<Response<ExistsResponse>, Status> {
        let path = request.get_ref().path.clone();
        self.authorize(&request, &path, false)?;
        let exists = self.workspace.exists(&path).await.map_err(status)?;
        Ok(Response::new(ExistsResponse { exists }))
    }

    async fn list(
        &self,
        request: Request<ListRequest>,
    ) -> std::result::Result<Response<ListResponse>, Status> {
        let dir = request.get_ref().dir.clone();
        self.authorize(&request, &dir, false)?;
        let entries = self.workspace.list(&dir).await.map_err(status)?;
        Ok(Response::new(ListResponse {
            entries: entries
                .into_iter()
                .map(|entry| WireEntry {
                    name: entry.name,
                    is_dir: entry.is_dir,
                    mode: entry.mode,
                    size: entry.size as i64,
                    mod_time_unix_nano: entry.modified_unix_nanos,
                })
                .collect(),
        }))
    }
}
