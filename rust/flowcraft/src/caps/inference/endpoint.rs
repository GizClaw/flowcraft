//! The inference endpoint: the capability over gRPC.
//!
//! Two things this service owns and the provider does not: **the caller's scope** — which
//! models and tiers a grant allows, and the output ceiling it imposes — and **the stream
//! shape**, which on the wire is a server stream rather than a byte stream.
//!
//! A tier or a hint is answered `NotAvailable` here rather than resolved: routing is its own
//! layer, and an endpoint that guessed what a tier meant would be the second owner of a
//! policy the deployment wrote once.

use std::sync::Arc;

use crate::client::grpc::{authenticate, grant_of, status};
use crate::error::{Error, Result};
use crate::kernel::handles::contracts;
use crate::kernel::{Endpoint as EndpointDescriptor, EndpointHost, Grants};
use crate::proto::inference as pb;
use crate::proto::inference::inference_server;
use crate::resource::Kind;
use crate::resource::{BoxFuture, BuildCtx, Factory, Phase, Resource, Spec, require};
use serde::Deserialize;
use serde_json::Value;
use tokio::sync::mpsc;
use tokio_stream::wrappers::ReceiverStream;
use tonic::service::Interceptor;
use tonic::{Request, Response, Status};

use crate::caps::inference::contract::{CONTRACT, Handle};
use crate::caps::inference::scope::Scope;
use crate::inference::wire;

/// The endpoint's registry kind.
pub const KIND: &str = "inference.Endpoint";
/// The endpoint's cross-process contract identity.
pub const CONTRACT_ENDPOINT: &str = "inference.Endpoint/v1";
const IMPL: &str = "grpc";

/// How many stream events may sit between the provider and the socket.
///
/// A slow client then pushes back on the provider instead of growing a buffer: the whole
/// reason streaming exists is that the answer does not have to fit in memory.
const STREAM_QUEUE: usize = 16;

/// The `inference.Endpoint/grpc` service.
pub struct Endpoint {
    provider: Handle,
    host: EndpointHost,
    grants: Grants,
    descriptor: EndpointDescriptor,
    capability: String,
    serving: std::sync::Mutex<Option<crate::kernel::Serving>>,
}

impl std::fmt::Debug for Endpoint {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("inference.Endpoint")
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
                provider: Handle::new(self.provider.provider()),
            };
            let interceptor = GrantInterceptor {
                grants: self.grants.clone(),
                descriptor: self.descriptor.clone(),
                capability: self.capability.clone(),
            };
            let router = server.add_service(inference_server::InferenceServer::with_interceptor(
                service,
                interceptor,
            ));
            let mut serving = self
                .serving
                .lock()
                .map_err(|_| Error::internal("inference endpoint lock poisoned"))?;
            *serving = Some(crate::kernel::Serving::spawn(router, listener));
            Ok(())
        })
    }

    fn shutdown(self: Arc<Self>) -> BoxFuture<'static, Result<()>> {
        Box::pin(async move {
            let handle = self
                .serving
                .lock()
                .map_err(|_| Error::internal("inference endpoint lock poisoned"))?
                .take();
            if let Some(serving) = handle {
                serving.shutdown().await?;
            }
            Ok(())
        })
    }
}

/// Authenticates the bearer grant and stashes it for the service.
///
/// The same interceptor the sandbox endpoint uses, and it belongs in `client::grpc` beside
/// `authenticate`; moving it is a separate refactor and this copy is four lines.
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
    provider: Handle,
}

impl Service {
    fn scope<T>(&self, request: &Request<T>) -> std::result::Result<Scope, Status> {
        let grant = grant_of(request)?;
        if grant.capability != CONTRACT {
            return Err(Status::permission_denied("grant capability mismatch"));
        }
        Scope::parse(&grant.scope).map_err(|_| Status::permission_denied("grant scope is invalid"))
    }

    /// The resolved model name a target asks for.
    ///
    /// A tier is *allowed* but not yet *resolvable*: routing is a later layer, and answering
    /// `NotAvailable` here says exactly that instead of silently treating a tier as a model
    /// name.
    fn resolve(
        &self,
        scope: &Scope,
        target: Option<pb::Target>,
    ) -> std::result::Result<String, Status> {
        let Some(choose) = target.and_then(|target| target.choose) else {
            return Err(Status::invalid_argument(
                "inference: a request must name a target",
            ));
        };
        match choose {
            pb::target::Choose::Model(model) => {
                let name = model
                    .id
                    .map(|id| id.name)
                    .filter(|name| !name.is_empty())
                    .ok_or_else(|| {
                        Status::invalid_argument("inference: the target names no model")
                    })?;
                if !scope.allows_model(&name) {
                    return Err(Status::permission_denied(format!(
                        "inference: this grant does not allow model {name:?}"
                    )));
                }
                Ok(name)
            }
            pb::target::Choose::Tier(tier) => {
                if !scope.allows_tier(&tier) {
                    return Err(Status::permission_denied(format!(
                        "inference: this grant does not allow tier {tier:?}"
                    )));
                }
                Err(Status::unimplemented(
                    "inference: routing is not implemented yet, so a tier cannot be resolved; \
                     name a model",
                ))
            }
            pb::target::Choose::ModelHint(hint) => Err(Status::unimplemented(format!(
                "inference: a hint ({hint:?}) is resolved by routing, which is not implemented yet"
            ))),
        }
    }

    /// Apply the grant's ceiling to a decoded request.
    fn apply_ceiling(
        scope: &Scope,
        request: &mut crate::inference::request::GenerateRequest,
    ) -> std::result::Result<(), Status> {
        request.intent.max_output_tokens = scope.output_limit(request.intent.max_output_tokens);
        Ok(())
    }
}

#[tonic::async_trait]
impl inference_server::Inference for Service {
    async fn models(
        &self,
        request: Request<pb::ModelsRequest>,
    ) -> std::result::Result<Response<pb::ModelsResponse>, Status> {
        let scope = self.scope(&request)?;
        let models = self.provider.models().await.map_err(status)?;
        // The catalogue *this caller may use*, not the catalogue that exists: a list of
        // names a grant cannot call is an invitation to a `permission_denied`.
        Ok(Response::new(pb::ModelsResponse {
            models: models
                .into_iter()
                .filter(|model| scope.allows_model(&model.name))
                .map(|model| pb::ModelDescriptor {
                    id: Some(pb::ModelId {
                        provider: model.provider,
                        name: model.name,
                    }),
                    family: String::new(),
                    input_kinds: vec!["text".to_owned()],
                    output_kinds: vec!["text".to_owned()],
                    operations: vec!["generate".to_owned()],
                })
                .collect(),
        }))
    }

    async fn explain(
        &self,
        request: Request<pb::ExplainRequest>,
    ) -> std::result::Result<Response<pb::ExplainResponse>, Status> {
        let scope = self.scope(&request)?;
        let mut inner = request.into_inner();
        let model = self.resolve(&scope, inner.target.take())?;
        let mut request = crate::inference::request::GenerateRequest {
            model: model.clone(),
            messages: inner.messages,
            intent: inner
                .intent
                .map(wire::intent_from_proto)
                .unwrap_or_default(),
        };
        Service::apply_ceiling(&scope, &mut request)?;
        let report = self.provider.explain(&request).await.map_err(status)?;
        Ok(Response::new(pb::ExplainResponse {
            resolved: Some(pb::ModelRef {
                id: Some(pb::ModelId {
                    provider: String::new(),
                    name: model,
                }),
            }),
            report: Some(wire::report_to_proto(&report)),
        }))
    }

    async fn generate(
        &self,
        request: Request<pb::GenerateRequest>,
    ) -> std::result::Result<Response<pb::GenerateResponse>, Status> {
        let scope = self.scope(&request)?;
        let mut inner = request.into_inner();
        let model = self.resolve(&scope, inner.target.take())?;
        let mut request = crate::inference::request::GenerateRequest {
            model,
            messages: inner.messages,
            intent: inner
                .intent
                .map(wire::intent_from_proto)
                .unwrap_or_default(),
        };
        Service::apply_ceiling(&scope, &mut request)?;
        let generation = self.provider.generate(request).await.map_err(status)?;
        Ok(Response::new(
            crate::caps::inference::contract::generation_to_proto(&generation),
        ))
    }

    type GenerateStreamStream = ReceiverStream<std::result::Result<pb::GenerateEvent, Status>>;

    async fn generate_stream(
        &self,
        request: Request<pb::GenerateStreamRequest>,
    ) -> std::result::Result<Response<Self::GenerateStreamStream>, Status> {
        let scope = self.scope(&request)?;
        let pb::GenerateStreamRequest { request } = request.into_inner();
        let inner =
            request.ok_or_else(|| Status::invalid_argument("inference: request is required"))?;
        let model = self.resolve(&scope, inner.target.clone())?;
        let mut decoded = crate::inference::request::GenerateRequest {
            model: model.clone(),
            messages: inner.messages,
            intent: inner
                .intent
                .map(wire::intent_from_proto)
                .unwrap_or_default(),
        };
        Service::apply_ceiling(&scope, &mut decoded)?;
        // The stream is opened before the first event is produced, so a caller learns the
        // model and then waits: `Started` is the endpoint's own event, not the provider's.
        let (sender, receiver) = mpsc::channel(STREAM_QUEUE);
        let provider = self.provider.provider();
        tokio::spawn(async move {
            let started = pb::GenerateEvent {
                kind: Some(pb::generate_event::Kind::Started(pb::Started {
                    model: Some(pb::ModelId {
                        provider: String::new(),
                        name: model.clone(),
                    }),
                    response_id: String::new(),
                })),
            };
            if sender.send(Ok(started)).await.is_err() {
                return;
            }
            let mut events = match provider.generate_stream(decoded).await {
                Ok(events) => events,
                Err(error) => {
                    let _ = sender.send(Err(status(error))).await;
                    return;
                }
            };
            use tokio_stream::StreamExt as _;
            while let Some(event) = events.next().await {
                let encoded = match event {
                    Ok(event) => Ok(wire::event_to_proto(&event, None)),
                    Err(error) => Err(status(error)),
                };
                if sender.send(encoded).await.is_err() {
                    // The caller went away: dropping the provider stream cancels the read.
                    return;
                }
            }
        });
        Ok(Response::new(ReceiverStream::new(receiver)))
    }
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct Settings {
    /// The endpoint this service is served on, as named in `platform.yaml`.
    endpoint: String,
    /// The capability identity, when it is not [`CONTRACT`].
    #[serde(default)]
    capability: Option<String>,
}

/// The `inference.Endpoint/grpc` factory.
pub struct EndpointFactory;

impl Factory for EndpointFactory {
    type Output = Endpoint;

    fn spec(&self) -> Spec {
        let kind = Kind::new(KIND).unwrap_or_else(|error| panic!("{KIND} is invalid: {error}"));
        let mut provider = require::<Handle>("provider");
        provider.contract = Some(CONTRACT);
        let mut host = require::<EndpointHost>("host");
        host.contract = Some(contracts::ENDPOINT_HOST);
        let mut grants = require::<Grants>("grants");
        grants.contract = Some(contracts::GRANTS);
        Spec {
            kind,
            imp: Some(IMPL),
            contract: Some(CONTRACT_ENDPOINT),
            deps: vec![provider, host, grants],
            phase: Phase::Main,
            validate_settings: Some(validate),
        }
    }

    fn build<'a>(&'a self, ctx: BuildCtx<'a>) -> BoxFuture<'a, Result<Arc<Self::Output>>> {
        Box::pin(async move {
            let settings: Settings = ctx.settings()?;
            let host = ctx.deps.get::<EndpointHost>("host")?;
            let grants = ctx.deps.get::<Grants>("grants")?;
            let provider = ctx.deps.get::<Handle>("provider")?;
            let descriptor = host.catalog().resolve(&settings.endpoint)?;
            let capability = settings.capability.unwrap_or_else(|| CONTRACT.to_owned());
            if !descriptor.supports(&capability) {
                return Err(Error::not_available(format!(
                    "endpoint {:?} does not serve {capability:?}",
                    descriptor.name
                )));
            }
            Ok(Arc::new(Endpoint {
                provider: Handle::new(provider.provider()),
                host: (*host).clone(),
                grants: (*grants).clone(),
                descriptor,
                capability,
                serving: std::sync::Mutex::new(None),
            }))
        })
    }
}

fn validate(value: &Value) -> Result<()> {
    let settings: Settings = crate::resource::settings(value)?;
    if settings.endpoint.trim().is_empty() {
        return Err(Error::validation(
            "inference: settings.endpoint is required",
        ));
    }
    Ok(())
}
