//! The tool DTOs a chat turn carries: what a tool is ([`ToolDefinition`]),
//! what the model asked it to do ([`ToolCall`]), and what came back
//! ([`ToolResult`]).
//!
//! The two raw payloads -- a definition's input schema and a call's arguments
//! -- are JSON objects kept as text, the port's `json.RawMessage`. They are
//! validated (`{` and well-formed) and otherwise opaque: the message layer
//! never looks inside what a tool declared or what the model passed it.

use crate::json;
use crate::message::Content;
use crate::text;

/// A callable tool: a name, a description, and a JSON Schema for its
/// arguments.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct ToolDefinition {
    /// The tool's name, as the model calls it.
    pub name: String,
    /// What the tool does, in the words the model sees.
    pub description: String,
    /// A JSON Schema object describing the arguments.
    pub input_schema: String,
}

impl ToolDefinition {
    /// A definition over an input schema given as text.
    pub fn new(
        name: impl Into<String>,
        description: impl Into<String>,
        input_schema: impl Into<String>,
    ) -> Self {
        ToolDefinition {
            name: name.into(),
            description: description.into(),
            input_schema: input_schema.into(),
        }
    }

    /// core's `ToolDefinition.Validate`: a name, and a schema that is a JSON
    /// object rather than an untyped map a provider would have to guess at.
    pub fn validate(&self) -> Result<(), String> {
        if self.name.is_empty() {
            return Err("tool definition name is required".into());
        }
        if !is_json_object(&self.input_schema) {
            return Err(format!(
                "tool {} input schema must be a JSON object",
                text::quote(&self.name)
            ));
        }
        Ok(())
    }

    /// The wire form. Like core's marshal, this does not validate: an empty
    /// schema is written as `null`.
    pub fn to_wire_json(&self) -> Result<String, String> {
        let mut out = String::from("{\"name\":");
        json::write_string(&self.name, &mut out);
        if !self.description.is_empty() {
            out.push_str(",\"description\":");
            json::write_string(&self.description, &mut out);
        }
        out.push_str(",\"input_schema\":");
        write_raw(&self.input_schema, &mut out)?;
        out.push('}');
        Ok(out)
    }
}

/// A tool invocation. `arguments` is always a JSON object and stays opaque to
/// everything above the tool boundary.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct ToolCall {
    /// The provider's identifier for this call, echoed back on the result.
    pub id: String,
    /// The tool being called.
    pub name: String,
    /// The arguments, as a JSON object in text.
    pub arguments: String,
}

impl ToolCall {
    /// core's `NewToolCall`: encode `arguments` and hold the result to the
    /// call rules.
    pub fn new(
        id: impl Into<String>,
        name: impl Into<String>,
        arguments: &serde_json::Value,
    ) -> Result<ToolCall, String> {
        let call = ToolCall {
            id: id.into(),
            name: name.into(),
            arguments: json::to_string(arguments),
        };
        call.validate()?;
        Ok(call)
    }

    /// core's `ToolCall.Validate`.
    pub fn validate(&self) -> Result<(), String> {
        if self.id.is_empty() {
            return Err("tool call id is required".into());
        }
        if self.name.is_empty() {
            return Err("tool call name is required".into());
        }
        if !is_json_object(&self.arguments) {
            return Err(format!(
                "tool call {} arguments must be a JSON object",
                text::quote(&self.id)
            ));
        }
        Ok(())
    }

    /// The strict decode of a `call` object: unknown fields refused, absent
    /// ones left empty, nothing validated.
    pub fn from_wire_json(document: &str) -> Result<ToolCall, String> {
        let members = json::Members::parse(document)?;
        if let Some(unknown) = members.first_unknown(&["id", "name", "arguments"]) {
            return Err(format!("json: unknown field {}", text::quote(unknown)));
        }
        Ok(ToolCall {
            id: members.string("id")?,
            name: members.string("name")?,
            arguments: members.raw("arguments").unwrap_or_default().to_owned(),
        })
    }

    /// The wire form.
    pub fn to_wire_json(&self) -> Result<String, String> {
        let mut out = String::from("{\"id\":");
        json::write_string(&self.id, &mut out);
        out.push_str(",\"name\":");
        json::write_string(&self.name, &mut out);
        out.push_str(",\"arguments\":");
        write_raw(&self.arguments, &mut out)?;
        out.push('}');
        Ok(out)
    }
}

/// One tool execution result, carried back into a chat turn.
///
/// The content is the ordered multimodal payload: a text-only tool answers with
/// a single text part, while image, audio, file and structured-data results
/// keep their typed parts instead of being flattened into a string at the tool
/// boundary.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct ToolResult {
    /// The call this answers.
    pub call_id: String,
    /// What the tool produced.
    pub content: Content,
    /// Whether the tool failed. A failure is model-visible content, not a
    /// diagnostic channel.
    pub is_error: bool,
}

impl ToolResult {
    /// core's `NewToolResult`: a call id and its content.
    pub fn new(call_id: impl Into<String>, content: Content) -> Self {
        ToolResult {
            call_id: call_id.into(),
            content,
            is_error: false,
        }
    }

    /// A successful result carrying one text part.
    pub fn text(call_id: impl Into<String>, text: impl Into<String>) -> Self {
        ToolResult::new(call_id, Content::text(text))
    }

    /// A failed result carrying one text part -- the message the model reads,
    /// not a diagnostic dump.
    pub fn error(call_id: impl Into<String>, text: impl Into<String>) -> Self {
        ToolResult {
            call_id: call_id.into(),
            content: Content::text(text),
            is_error: true,
        }
    }

    /// core's `ToolResult.Validate`.
    pub fn validate(&self) -> Result<(), String> {
        if self.call_id.is_empty() {
            return Err("tool result call id is required".into());
        }
        self.content
            .validate()
            .map_err(|err| format!("tool result {}: {err}", text::quote(&self.call_id)))
    }

    /// The strict decode of a `result` object: unknown fields refused, an
    /// absent `content` left empty, nothing validated.
    pub fn from_wire_json(document: &str) -> Result<ToolResult, String> {
        let members = json::Members::parse(document)?;
        if let Some(unknown) = members.first_unknown(&["call_id", "content", "is_error"]) {
            return Err(format!("json: unknown field {}", text::quote(unknown)));
        }
        let content = match members.raw("content") {
            Some(raw) => Content::from_wire_json(raw)?,
            None => Content::default(),
        };
        Ok(ToolResult {
            call_id: members.string("call_id")?,
            content,
            is_error: members.bool("is_error")?,
        })
    }

    /// The wire form; the content validates, so an unbuildable result cannot be
    /// written.
    pub fn to_wire_json(&self) -> Result<String, String> {
        let mut out = String::from("{\"call_id\":");
        json::write_string(&self.call_id, &mut out);
        out.push_str(",\"content\":");
        out.push_str(&self.content.to_wire_json()?);
        if self.is_error {
            out.push_str(",\"is_error\":true");
        }
        out.push('}');
        Ok(out)
    }
}

/// `isJSONObject`: a value that starts like an object and parses.
pub(crate) fn is_json_object(raw: &str) -> bool {
    let raw = raw.trim();
    raw.starts_with('{') && json::is_valid(raw)
}

/// Writes a raw payload the way `json.RawMessage` is written: `null` when
/// empty, otherwise compacted with the text's own key order.
fn write_raw(raw: &str, out: &mut String) -> Result<(), String> {
    if raw.is_empty() {
        out.push_str("null");
        return Ok(());
    }
    if !json::is_valid(raw) {
        return Err("json: raw payload is not valid JSON".into());
    }
    out.push_str(&json::compact(raw));
    Ok(())
}
