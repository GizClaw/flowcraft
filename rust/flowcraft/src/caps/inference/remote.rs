//! The remote inference client: the same contract, on the other side of a connection.
//!
//! It exists for one reason the design names: the Go runtime keeps its own routing for now,
//! so a Go deployment reaches this tree's pipeline through a driver that is a gRPC client.
//! Everything a caller sees is [`Inference`] — the wire and the grant are this module's
//! problem.
//!
//! Two shapes it has to translate, and both are the wire's rather than the contract's:
//! `Started` (the server opening a stream) has no canonical event, so it is dropped; and a
//! `Streaming` item is a `Status`, which becomes the canonical error at the boundary.

use std::sync::Arc;

use async_trait::async_trait;
use futures_util::StreamExt as _;
use serde::Deserialize;
use serde_json::Value;

use crate::caps::inference::contract::{
    CONTRACT, Capabilities, EventStream, Inference, ModelDescriptor,
};
use crate::client::grpc::from_status;
use crate::client::wire::Wire;
use crate::error::{Error, Result};
use crate::inference::model::{CompileReport, Operation};
use crate::inference::pipeline::Generation;
use crate::inference::request::GenerateRequest;
use crate::inference::wire;
use crate::kernel::{EndpointProvider, Endpoints};
use crate::proto::inference as pb;
use crate::proto::inference::inference_client::InferenceClient;
use crate::resource::Kind;
use crate::resource::{BoxFuture, BuildCtx, Factory, Phase, Spec, require};

/// The implementation name.
const IMPL: &str = "remote";

/// A provider reached over gRPC.
pub struct Remote {
    wire: Arc<Wire>,
}

impl std::fmt::Debug for Remote {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("inference.Remote").finish_non_exhaustive()
    }
}

impl Remote {
    /// Mint a grant and prepare a client. Capabilities are asked for on demand: build runs
    /// before start, so the endpoint may not be listening yet.
    pub async fn connect(
        provider: Arc<dyn EndpointProvider>,
        endpoint: &str,
        tenant: &str,
        scope: Value,
    ) -> Result<Self> {
        Ok(Self {
            wire: Arc::new(Wire::open(provider, endpoint, CONTRACT, tenant, scope).await?),
        })
    }
}

#[async_trait]
impl Inference for Remote {
    fn capabilities(&self) -> BoxFuture<'_, Result<Capabilities>> {
        Box::pin(async move {
            let mut client = InferenceClient::new(self.wire.channel().await?);
            let response = client
                .describe(self.wire.request(pb::DescribeRequest {}).await?)
                .await
                .map_err(from_status)?
                .into_inner();
            let mut operations = Vec::with_capacity(response.operations.len());
            for raw in response.operations {
                operations.push(Operation::parse(&raw)?);
            }
            Ok(Capabilities {
                operations,
                streams: response.streams,
                models: usize::try_from(response.models).unwrap_or(usize::MAX),
            })
        })
    }

    async fn models(&self) -> Result<Vec<ModelDescriptor>> {
        let mut client = InferenceClient::new(self.wire.channel().await?);
        let response = client
            .models(self.wire.request(pb::ModelsRequest {}).await?)
            .await
            .map_err(from_status)?
            .into_inner();
        Ok(response
            .models
            .into_iter()
            .map(|model| {
                let id = model.id.unwrap_or_default();
                ModelDescriptor {
                    name: id.name,
                    provider: id.provider,
                    // The wire's descriptor names operations, not a dialect; the dialect is
                    // the server's business and a client has no reason to know it.
                    api: String::new(),
                }
            })
            .collect())
    }

    async fn explain(&self, request: &GenerateRequest) -> Result<CompileReport> {
        let encoded = wire::request_to_proto(request);
        let mut client = InferenceClient::new(self.wire.channel().await?);
        let response = client
            .explain(
                self.wire
                    .request(pb::ExplainRequest {
                        target: encoded.target,
                        messages: encoded.messages,
                        intent: encoded.intent,
                    })
                    .await?,
            )
            .await
            .map_err(from_status)?
            .into_inner();
        wire::report_from_proto(response.report.unwrap_or_default())
    }

    async fn generate(&self, request: GenerateRequest) -> Result<Generation> {
        let encoded = wire::request_to_proto(&request);
        let mut client = InferenceClient::new(self.wire.channel().await?);
        let response = client
            .generate(self.wire.request(encoded).await?)
            .await
            .map_err(from_status)?
            .into_inner();
        let (response, report) = wire::response_from_proto(response)?;
        Ok(Generation { response, report })
    }

    async fn generate_stream(&self, request: GenerateRequest) -> Result<EventStream> {
        let encoded = wire::request_to_proto(&request);
        let mut client = InferenceClient::new(self.wire.channel().await?);
        let stream = client
            .generate_stream(
                self.wire
                    .request(pb::GenerateStreamRequest {
                        request: Some(encoded),
                    })
                    .await?,
            )
            .await
            .map_err(from_status)?
            .into_inner();
        Ok(Box::pin(stream.filter_map(|item| async move {
            match item {
                Ok(event) => match wire::event_from_proto(event) {
                    // `Started` and a delta this build does not lower are bookkeeping.
                    Ok(Some(event)) => Some(Ok(event)),
                    Ok(None) => None,
                    Err(error) => Some(Err(error)),
                },
                Err(status) => Some(Err(from_status(status))),
            }
        })))
    }
}

/// Settings for the remote provider.
#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct Settings {
    /// The endpoint this client dials, as named in `platform.yaml`.
    endpoint: String,
    /// The tenant the grant is minted for.
    #[serde(default)]
    tenant: String,
    /// The scope the grant carries: which models, tiers and ceiling this client may use.
    #[serde(default)]
    scope: Value,
}

/// The `inference.Provider/remote` factory.
pub struct RemoteFactory;

impl Factory for RemoteFactory {
    type Output = crate::caps::inference::contract::Handle;

    fn spec(&self) -> Spec {
        let kind = Kind::new(crate::caps::inference::contract::KIND)
            .unwrap_or_else(|error| panic!("invalid kind: {error}"));
        let mut endpoints = require::<Endpoints>("endpoints");
        endpoints.contract = Some(crate::kernel::handles::contracts::ENDPOINT_PROVIDER);
        Spec {
            kind,
            imp: Some(IMPL),
            contract: Some(CONTRACT),
            deps: vec![endpoints],
            phase: Phase::Main,
            validate_settings: Some(validate),
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
            let remote = Remote::connect(
                endpoints.0.clone(),
                &settings.endpoint,
                &settings.tenant,
                scope,
            )
            .await?;
            Ok(Arc::new(crate::caps::inference::contract::Handle::new(
                Arc::new(remote),
            )))
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
