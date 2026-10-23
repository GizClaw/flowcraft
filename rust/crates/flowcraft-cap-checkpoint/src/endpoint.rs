//! The endpoint server: a checkpoint store exposed over gRPC.

use std::sync::{Arc, Mutex};

use flowcraft_client::grpc::{authenticate, grant_of, status};
use flowcraft_error::{Error, Result};
use flowcraft_kernel::handles::contracts;
use flowcraft_kernel::{Endpoint as EndpointDescriptor, EndpointHost, Grants};
use flowcraft_proto::checkpoint::{
    DeleteRequest, DeleteResponse, ListRequest, ListResponse, LoadRequest, LoadResponse,
    SaveRequest, SaveResponse, checkpoint_server,
};
use flowcraft_resource::Kind;
use flowcraft_resource::{BoxFuture, BuildCtx, Factory, Phase, Resource, Spec, require};
use serde::Deserialize;
use serde_json::Value;
use tonic::service::Interceptor;
use tonic::{Request, Response, Status};

use crate::contract::{CONTRACT, Handle, extract_exec_id};
use crate::scope::Scope;

/// The registry kind.
pub const KIND: &str = "checkpoint.Endpoint";
/// The implementation name.
pub const IMPL: &str = "grpc";
/// The capability this endpoint serves by default.
pub const CAPABILITY: &str = "checkpoint.Store/v1";

/// The resource's own contract identity.
pub const CONTRACT_ENDPOINT: &str = "checkpoint.Endpoint/v1";

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct Settings {
    endpoint: String,
    #[serde(default)]
    capability: Option<String>,
}

fn validate(value: &Value) -> Result<()> {
    let settings: Settings = flowcraft_resource::settings(value)?;
    if settings.endpoint.trim().is_empty() {
        return Err(Error::validation("settings.endpoint is required"));
    }
    Ok(())
}

/// A resource that serves a checkpoint store on the endpoint named in its
/// settings.
pub struct Endpoint {
    store: Handle,
    host: EndpointHost,
    grants: Grants,
    descriptor: EndpointDescriptor,
    capability: String,
    serving: Mutex<Option<flowcraft_kernel::Serving>>,
}

impl std::fmt::Debug for Endpoint {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("checkpoint.Endpoint")
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
                store: self.store.clone(),
                descriptor: self.descriptor.clone(),
                capability: self.capability.clone(),
            };
            let interceptor = GrantInterceptor {
                grants: self.grants.clone(),
                descriptor: self.descriptor.clone(),
                capability: self.capability.clone(),
            };
            let router = server.add_service(checkpoint_server::CheckpointServer::with_interceptor(
                service,
                interceptor,
            ));
            let mut serving = self
                .serving
                .lock()
                .map_err(|_| Error::internal("endpoint lock poisoned"))?;
            *serving = Some(flowcraft_kernel::Serving::spawn(router, listener));
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

/// The `checkpoint.Endpoint/grpc` factory.
pub struct EndpointFactory;

impl Factory for EndpointFactory {
    type Output = Endpoint;

    fn spec(&self) -> Spec {
        let kind = Kind::new(KIND).unwrap_or_else(|error| panic!("{KIND} is invalid: {error}"));
        let mut store = require::<Handle>("store");
        store.contract = Some(CONTRACT);
        let mut host = require::<EndpointHost>("host");
        host.contract = Some(contracts::ENDPOINT_HOST);
        let mut grants = require::<Grants>("grants");
        grants.contract = Some(contracts::GRANTS);
        Spec {
            contract: Some(CONTRACT_ENDPOINT),
            imp: Some(IMPL),
            deps: vec![store, host, grants],
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
            let store = ctx.deps.get::<Handle>("store")?;

            let descriptor = host.catalog().resolve(&settings.endpoint)?;
            let capability = settings.capability.unwrap_or_else(|| CONTRACT.to_owned());
            if !descriptor.supports(&capability) {
                return Err(Error::not_available(format!(
                    "endpoint {:?} does not serve {capability:?}",
                    descriptor.name
                )));
            }

            Ok(Arc::new(Endpoint {
                store: Handle::new(store.store()),
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
    store: Handle,
    descriptor: EndpointDescriptor,
    capability: String,
}

impl Service {
    fn scope<T>(&self, request: &Request<T>) -> std::result::Result<Scope, Status> {
        let grant = grant_of(request)?;
        self.descriptor.authorize(&grant).map_err(status)?;
        if grant.capability != self.capability {
            return Err(Status::permission_denied("grant capability mismatch"));
        }
        Scope::parse(&grant.scope).map_err(|_| Status::permission_denied("grant scope is invalid"))
    }
}

#[tonic::async_trait]
impl checkpoint_server::Checkpoint for Service {
    async fn save(
        &self,
        request: Request<SaveRequest>,
    ) -> std::result::Result<Response<SaveResponse>, Status> {
        let scope = self.scope(&request)?;
        let checkpoint = request.get_ref().checkpoint.clone();
        let exec_id = extract_exec_id(&checkpoint).map_err(status)?;
        scope.allows(&exec_id).map_err(status)?;
        self.store
            .save(&exec_id, &checkpoint)
            .await
            .map_err(status)?;
        Ok(Response::new(SaveResponse {}))
    }

    async fn load(
        &self,
        request: Request<LoadRequest>,
    ) -> std::result::Result<Response<LoadResponse>, Status> {
        let scope = self.scope(&request)?;
        let exec_id = request.get_ref().exec_id.clone();
        scope.allows(&exec_id).map_err(status)?;
        match self.store.load(&exec_id).await.map_err(status)? {
            Some(checkpoint) => Ok(Response::new(LoadResponse {
                checkpoint,
                found: true,
            })),
            None => Ok(Response::new(LoadResponse {
                checkpoint: Vec::new(),
                found: false,
            })),
        }
    }

    async fn list(
        &self,
        request: Request<ListRequest>,
    ) -> std::result::Result<Response<ListResponse>, Status> {
        let scope = self.scope(&request)?;
        let ids = self.store.list().await.map_err(status)?;
        Ok(Response::new(ListResponse {
            exec_ids: scope.filter(ids),
        }))
    }

    async fn delete(
        &self,
        request: Request<DeleteRequest>,
    ) -> std::result::Result<Response<DeleteResponse>, Status> {
        let scope = self.scope(&request)?;
        let exec_id = request.get_ref().exec_id.clone();
        scope.allows(&exec_id).map_err(status)?;
        self.store.delete(&exec_id).await.map_err(status)?;
        Ok(Response::new(DeleteResponse {}))
    }
}
