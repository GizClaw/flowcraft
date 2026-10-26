//! The endpoint server: a secret store exposed over gRPC.

use std::sync::{Arc, Mutex};

use crate::client::grpc::{authenticate, grant_of, status};
use crate::error::{Error, Result};
use crate::kernel::handles::contracts;
use crate::kernel::{Endpoint as EndpointDescriptor, EndpointHost, Grants};
use crate::proto::secret::{LookupRequest, LookupResponse, secret_server};
use crate::resource::Kind;
use crate::resource::{BoxFuture, BuildCtx, Factory, Phase, Resource, Spec, require};
use serde::Deserialize;
use serde_json::Value;
use tonic::service::Interceptor;
use tonic::{Request, Response, Status};

use crate::caps::secret::contract::{CONTRACT, Handle};
use crate::caps::secret::scope::Scope;

/// The registry kind.
pub const KIND: &str = "secret.Endpoint";
/// The implementation name.
pub const IMPL: &str = "grpc";
/// The capability this endpoint serves by default.
pub const CAPABILITY: &str = "secret.Store/v1";

/// The resource's own contract identity.
pub const CONTRACT_ENDPOINT: &str = "secret.Endpoint/v1";

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

/// A resource that serves a secret store on the endpoint named in its settings.
pub struct Endpoint {
    store: Handle,
    host: EndpointHost,
    grants: Grants,
    descriptor: EndpointDescriptor,
    capability: String,
    serving: Mutex<Option<crate::kernel::Serving>>,
}

impl std::fmt::Debug for Endpoint {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("secret.Endpoint")
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
            let router = server.add_service(secret_server::SecretServer::with_interceptor(
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

/// The `secret.Endpoint/grpc` factory.
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

#[tonic::async_trait]
impl secret_server::Secret for Service {
    async fn lookup(
        &self,
        request: Request<LookupRequest>,
    ) -> std::result::Result<Response<LookupResponse>, Status> {
        let grant = grant_of(&request)?;
        self.descriptor.authorize(&grant).map_err(status)?;
        if grant.capability != self.capability {
            return Err(Status::permission_denied("grant capability mismatch"));
        }
        let scope = Scope::parse(&grant.scope)
            .map_err(|_| Status::permission_denied("grant scope is invalid"))?;

        let name = request.get_ref().name.clone();
        scope.allows(&name).map_err(status)?;

        match self.store.lookup(&name).await.map_err(status)? {
            Some(value) => Ok(Response::new(LookupResponse { value, found: true })),
            None => Ok(Response::new(LookupResponse {
                value: String::new(),
                found: false,
            })),
        }
    }
}
