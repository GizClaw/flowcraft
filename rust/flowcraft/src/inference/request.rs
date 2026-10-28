//! The canonical request, and what one compile produced.
//!
//! The content layer is [`crate::proto::message`] — the same `Message`/`Content`/
//! `Part` the wire uses — because a second, in-process copy of that vocabulary is how
//! the two drift. What this module adds is the **intent** layer: what the caller wants
//! done with that content, and the evidence a compiler produced for it.
//!
//! The wire contract (`inference/v1`) will mirror these types field for field; until
//! it exists this is the only definition, which is why every field here carries the
//! reason it exists rather than a description of its name.

use serde::{Deserialize, Serialize};

use crate::proto::message as msg;

/// One request to a generation model.
#[derive(Debug, Clone, PartialEq)]
pub struct GenerateRequest {
    /// The model this call is meant for. Routing (a `tier`) is the platform's job and
    /// arrives later; a driver only ever sees a resolved model.
    pub model: String,
    /// The conversation so far, oldest first.
    pub messages: Vec<msg::Message>,
    /// What to do with it.
    pub intent: TextIntent,
}

/// The text-generation half of the intent layer.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct TextIntent {
    /// Hard ceiling on the completion, in tokens.
    pub max_output_tokens: Option<u64>,
    /// Sampling temperature.
    pub temperature: Option<f64>,
    /// Tools the model may call.
    pub tools: Vec<msg::ToolDefinition>,
    /// How the model should choose among them.
    pub tool_choice: ToolChoice,
    /// The shape the answer must have.
    pub format: TextFormat,
    /// Reasoning effort, for models that expose one.
    pub reasoning_effort: Option<String>,
}

/// How a model picks a tool.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum ToolChoice {
    /// The model decides.
    #[default]
    Auto,
    /// It must call some tool.
    Required,
    /// It must not call one.
    None,
}

/// The required shape of the answer.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub enum TextFormat {
    /// Prose.
    #[default]
    Text,
    /// A JSON object, schema unchecked.
    JsonObject,
    /// A JSON object that must match `schema` (a JSON Schema document).
    JsonSchema {
        /// The name the provider files the schema under.
        name: String,
        /// The schema itself, serialized.
        schema: String,
    },
}

/// One compile: the provider-native body, and the evidence for it.
///
/// The body is a [`serde_json::Value`] rather than a typed struct per provider
/// because a wire dialect is exactly a JSON document with provider-specific quirks, and
/// the adapter that knows them is the one that builds it. What is typed is the
/// *report*, which is the part that travels.
#[derive(Debug, Clone, PartialEq)]
pub struct Compiled {
    /// The body to POST.
    pub body: serde_json::Value,
    /// Per-field evidence. A successful compile must cover every active field; a
    /// rejection means the caller's request cannot be honoured as written.
    pub report: crate::inference::model::CompileReport,
}

/// Why generation stopped.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum FinishReason {
    /// The model produced a complete answer.
    Stop,
    /// It hit the token ceiling.
    Length,
    /// It wants a tool call to happen.
    ToolCalls,
    /// Something else the provider named; kept verbatim by the adapter's caller.
    Other,
}

/// What one generation returned.
#[derive(Debug, Clone, PartialEq)]
pub struct GenerateResponse {
    /// The answer, as canonical parts.
    pub parts: Vec<msg::Part>,
    /// Why it stopped.
    pub finish: FinishReason,
    /// What it cost, in the provider's own counts.
    pub usage: crate::inference::usage::Usage,
}
