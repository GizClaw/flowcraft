//! The inference capability: a deployment's model table, served over the pipeline.
//!
//! Four segments, as every capability has: the contract, a local provider, an endpoint and
//! a remote client. This tree has the first two; the endpoint and the remote client land
//! with the wire conformance work.

pub mod contract;
pub mod local;

pub use contract::{CONTRACT, Capabilities, EventStream, Handle, Inference, KIND, ModelDescriptor};
pub use local::{Local, LocalFactory};
