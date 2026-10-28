//! The inference capability contract.
//!
//! One [`Inference`] serves a deployment's models: a catalogue, a compile that does not
//! execute, unary generation and a stream. Everything a backend can differ on is declared
//! once ([`Capabilities`]) rather than discovered by trying an operation and reading the
//! error.
//!
//! The *target* is already resolved to a model by the time it reaches an
//! [`Inference`]: a `tier` or a `model_hint` belongs to routing, and a capability that
//! guessed what a tier meant would be the second owner of a policy the deployment wrote
//! once.

use std::sync::Arc;

use async_trait::async_trait;
use tokio_stream::Stream;

use crate::error::Result;
use crate::inference::model::{CompileReport, Operation};
use crate::inference::pipeline::Generation;
use crate::inference::request::{GenerateEvent, GenerateRequest};
use crate::inference::wire;
use crate::resource::Resource;

/// The registry kind.
pub const KIND: &str = "inference.Provider";
/// The cross-process contract identity.
pub const CONTRACT: &str = "inference.Provider/v1";

/// What a provider serves.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize)]
pub struct Capabilities {
    /// The workloads it answers. Unary and streaming generation are one operation
    /// (`generate`); a provider that serves only the unary shape says so with
    /// [`Self::streams`].
    pub operations: Vec<Operation>,
    /// Whether it can answer a streaming request. A caller that asked for a stream from a
    /// provider without one gets `NotAvailable`, not a one-event stream pretending to be
    /// the real thing.
    pub streams: bool,
    /// How many models the deployment declared.
    pub models: usize,
}

/// One model a deployment declared.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize)]
pub struct ModelDescriptor {
    /// The name a request uses.
    pub name: String,
    /// The provider adapter that serves it (`openai`).
    pub provider: String,
    /// The dialect (`response`).
    pub api: String,
}

/// A stream of generation events.
pub type EventStream = std::pin::Pin<Box<dyn Stream<Item = Result<GenerateEvent>> + Send>>;

/// The capability.
#[async_trait]
pub trait Inference: Send + Sync {
    /// What this provider serves, and how many models it was given.
    ///
    /// Asynchronous because a remote provider's answer is on the other side of a
    /// connection: a local one answers without suspending.
    fn capabilities(&self) -> crate::resource::BoxFuture<'_, Result<Capabilities>>;

    /// The catalogue, from the deployment document.
    async fn models(&self) -> Result<Vec<ModelDescriptor>>;

    /// Compile without executing: the same report [`Self::generate`] would produce.
    ///
    /// The report is the point — a caller can prove what a request would lose before it
    /// spends a call on it.
    async fn explain(&self, request: &GenerateRequest) -> Result<CompileReport>;

    /// One unary call.
    async fn generate(&self, request: GenerateRequest) -> Result<Generation>;

    /// One streaming call.
    async fn generate_stream(&self, request: GenerateRequest) -> Result<EventStream>;
}

/// The resource wrapper.
pub struct Handle(pub Arc<dyn Inference>);

impl Handle {
    /// Wrap a provider.
    #[must_use]
    pub fn new(provider: Arc<dyn Inference>) -> Self {
        Self(provider)
    }

    /// The provider.
    #[must_use]
    pub fn provider(&self) -> Arc<dyn Inference> {
        Arc::clone(&self.0)
    }
}

impl std::fmt::Debug for Handle {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("inference.Handle")
    }
}

impl std::ops::Deref for Handle {
    type Target = Arc<dyn Inference>;

    fn deref(&self) -> &Self::Target {
        &self.0
    }
}

impl Resource for Handle {
    fn shutdown(self: Arc<Self>) -> crate::resource::BoxFuture<'static, Result<()>> {
        // A provider holds no background work: closing it is dropping it, and the
        // transport's connections go with it.
        Box::pin(async move { Ok(()) })
    }
}

/// The wire form of a generation, for a caller on the other side of a connection.
///
/// Kept here rather than in `inference::wire` so the capability owns the exact bytes a
/// peer receives: a change in the report's shape is a change to this function, and the
/// `cap-inference` endpoint calls it.
#[must_use]
pub fn generation_to_proto(generation: &Generation) -> crate::proto::inference::GenerateResponse {
    wire::response_to_proto(&generation.response, &generation.report)
}
