//! The inference capability: a deployment's model table, served over the pipeline.
//!
//! Four segments, as every capability has: the contract, a local provider, an endpoint and
//! a remote client. This tree has the first two; the endpoint and the remote client land
//! with the wire conformance work.

pub mod contract;
#[cfg(feature = "wire")]
pub mod endpoint;
pub mod local;
#[cfg(feature = "wire")]
pub mod remote;
pub mod scope;

pub use contract::{CONTRACT, Capabilities, EventStream, Handle, Inference, KIND, ModelDescriptor};
#[cfg(feature = "wire")]
pub use endpoint::{CONTRACT_ENDPOINT, Endpoint, EndpointFactory};
pub use local::{Local, LocalFactory};
#[cfg(feature = "wire")]
pub use remote::{Remote, RemoteFactory};
pub use scope::Scope;
