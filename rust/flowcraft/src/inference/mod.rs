//! The inference vocabulary and pipeline, between the wire and the capabilities.
//!
//! This layer knows what a canonical request *is* — its fields, its intents, what a
//! compile did, what a call cost — and nothing about deployments or transports. A
//! caller that only needs to name a model compiles this and no wire stack; a driver
//! implements the compile/execute contract without the control plane.

pub mod active;
#[cfg(feature = "inference-http")]
pub mod http;
pub mod ledger;
pub mod model;
pub mod openai;
pub mod pipeline;
pub mod request;
pub mod transport;
pub mod usage;

pub use active::active_fields;
#[cfg(feature = "inference-http")]
pub use http::HttpTransport;
pub use ledger::{Attempt, Ledger, Outcome, Phase, Trigger};
pub use model::{CompileReport, Decision, Disposition, FieldId, Operation};
pub use pipeline::{Driver, Generation, generate};
pub use request::{
    Compiled, FinishReason, GenerateRequest, GenerateResponse, TextFormat, TextIntent, ToolChoice,
};
pub use transport::Transport;
pub use usage::{Money, Usage};
