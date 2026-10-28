//! The inference vocabulary and pipeline, between the wire and the capabilities.
//!
//! This layer knows what a canonical request *is* — its fields, its intents, what a
//! compile did, what a call cost — and nothing about deployments or transports. A
//! caller that only needs to name a model compiles this and no wire stack; a driver
//! implements the compile/execute contract without the control plane.

pub mod model;
pub mod openai;
pub mod request;
pub mod usage;

pub use model::{CompileReport, Decision, Disposition, FieldId, Operation};
pub use request::{
    Compiled, FinishReason, GenerateRequest, GenerateResponse, TextFormat, TextIntent, ToolChoice,
};
pub use usage::{Money, Usage};
