//! The provider-neutral wire DTOs shared by every chat turn: a role-tagged
//! [`Message`] carrying an ordered [`Content`] of [`Part`]s, the tool-call
//! DTOs, and the JSON Schema helpers behind a tool definition.
//!
//! core keeps this in `core/message`, and with the same rule: no runtime, no
//! providers, no executors. It is the canonical shape of what travels on the
//! wire, so the layers above it (inference for LLM calls, tool for execution,
//! memory for stored turns) operate on these types without owning them, and
//! two consumers can share a message log without pulling in each other's
//! runtime.
//!
//! What lands when, and why the split is where it is:
//!
//! - the media DTOs are here (see [`media`]): they ride inside multimodal
//!   parts, so nothing above can be shaped without them;
//! - the part union ([`parts`]), the message and content types, the tool DTOs
//!   ([`tool`]) and the schema builders ([`schema`]) are here, and the codec is
//!   the reason this module exists: the wire format is JSON with each part
//!   tagged by its kind, and the port has to write the same bytes core writes
//!   and refuse the same documents core refuses;
//! - `message.Stream` (the delta transport inference streams through) and
//!   `MaterializeContent` land with the slice that consumes them: a stream is a
//!   runtime, and draining one belongs where the media transport lands.
//!
//! # Two asymmetries worth knowing before reading the codec
//!
//! `Message` itself has no custom decoder in core, so the *message* level is
//! permissive: unknown fields are ignored, a missing `content` is a zero
//! content rather than an error, and the role is not checked. `Content` and the
//! parts are strict and validate eagerly. The port keeps that split, because
//! callers on the other side of a wire depend on which documents get through.
//!
//! Marshal checks less than decode: a [`Message`] writes without validating its
//! role (core's `json.Marshal` does the same), while [`Content`] validates
//! before it writes. [`Message::validate`] is the call that runs both rule sets,
//! and it is what the runtime calls before a turn is stored or sent.

pub mod media;
pub mod parts;
pub mod schema;
pub mod tool;

use crate::json;
use crate::text;

use self::parts::Part;
pub use self::parts::{
    AudioPart, DataPart, FilePart, ImagePart, PartKind, ReasoningPart, TextPart, ToolCallPart,
    ToolResultPart, VideoPart,
};
pub use self::tool::{ToolCall, ToolDefinition, ToolResult};

/// Who a message is from.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Hash)]
pub enum Role {
    /// Instructions and context, ahead of the conversation.
    System,
    /// The human turn.
    #[default]
    User,
    /// The model's turn.
    Assistant,
    /// A turn carrying tool results back to the model.
    Tool,
}

impl Role {
    /// Every role, in core's declaration order.
    pub const ALL: [Role; 4] = [Role::System, Role::User, Role::Assistant, Role::Tool];

    /// The role's wire spelling.
    pub fn as_str(self) -> &'static str {
        match self {
            Role::System => "system",
            Role::User => "user",
            Role::Assistant => "assistant",
            Role::Tool => "tool",
        }
    }

    /// core's role check as a parse.
    pub fn parse(role: &str) -> Result<Role, String> {
        match role {
            "system" => Ok(Role::System),
            "user" => Ok(Role::User),
            "assistant" => Ok(Role::Assistant),
            "tool" => Ok(Role::Tool),
            other => Err(format!("unknown message role {}", text::quote(other))),
        }
    }
}

/// One turn: a role and the ordered parts that make it up.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct Message {
    /// Who the turn is from.
    pub role: Role,
    /// What it carries.
    pub content: Content,
}

impl Message {
    /// A message carrying a single text part.
    pub fn text(role: Role, text: impl Into<String>) -> Self {
        Message {
            role,
            content: Content::text(text),
        }
    }

    /// core's `Message.Validate`: the role is a role, the content is non-empty
    /// and valid, and the parts that only mean something in one role are in
    /// that role.
    pub fn validate(&self) -> Result<(), String> {
        if self.content.parts.is_empty() {
            return Err("message content is required".into());
        }
        self.content.validate()?;
        let mut has_tool_results = false;
        for part in &self.content.parts {
            match part {
                Part::ToolCall(_) if self.role != Role::Assistant => {
                    return Err("tool call parts require assistant role".into());
                }
                Part::ToolResult(_) => {
                    has_tool_results = true;
                    if self.role != Role::Tool {
                        return Err("tool result parts require tool role".into());
                    }
                }
                Part::Reasoning(_) if self.role != Role::Assistant => {
                    return Err("reasoning parts require assistant role".into());
                }
                _ => {}
            }
        }
        if self.role == Role::Tool && !has_tool_results {
            return Err("tool role requires a tool result part".into());
        }
        Ok(())
    }

    /// Every tool call in the message, in order.
    pub fn tool_calls(&self) -> Vec<ToolCall> {
        self.content
            .parts
            .iter()
            .filter_map(|part| match part {
                Part::ToolCall(part) => Some(part.call.clone()),
                _ => None,
            })
            .collect()
    }

    /// Every tool result in the message, in order.
    pub fn tool_results(&self) -> Vec<ToolResult> {
        self.content
            .parts
            .iter()
            .filter_map(|part| match part {
                Part::ToolResult(part) => Some(part.result.clone()),
                _ => None,
            })
            .collect()
    }

    /// Whether the message carries any tool call.
    pub fn has_tool_calls(&self) -> bool {
        self.content
            .parts
            .iter()
            .any(|part| matches!(part, Part::ToolCall(_)))
    }

    /// The wire form. The role is not validated (core writes an unknown role
    /// too); the content is, through [`Content::to_wire_json`].
    pub fn to_wire_json(&self) -> Result<String, String> {
        let mut out = String::from("{\"role\":");
        json::write_string(self.role.as_str(), &mut out);
        out.push_str(",\"content\":");
        out.push_str(&self.content.to_wire_json()?);
        out.push('}');
        Ok(out)
    }

    /// core's decode: permissive at this level (unknown fields ignored, absent
    /// fields left zero), strict from the content down.
    pub fn from_wire_json(document: &str) -> Result<Message, String> {
        let members = json::Members::parse(document)?;
        let role = Role::parse(&members.string("role")?)?;
        let content = match members.raw("content") {
            Some(raw) => Content::from_wire_json(raw)?,
            None => Content::default(),
        };
        Ok(Message { role, content })
    }
}

/// The last message in `messages` whose role is `role`.
pub fn last_by_role(messages: &[Message], role: Role) -> Option<&Message> {
    messages.iter().rev().find(|message| message.role == role)
}

/// An ordered collection of canonical parts.
///
/// Intent deliberately does not live here; only a request's input content
/// attaches execution intent. A content is never empty -- [`Content::validate`]
/// is what says so, and the codec refuses an empty one on both sides.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct Content {
    /// The parts, in order.
    pub parts: Vec<Part>,
}

impl Content {
    /// A content carrying a single text part -- the canonical way to express
    /// the "plain string result" that text-only producers still deal in.
    pub fn text(text: impl Into<String>) -> Self {
        Content {
            parts: vec![Part::Text(TextPart::new(text))],
        }
    }

    /// A content carrying one structured data part.
    ///
    /// A JSON object result is data, not prose: keeping it typed lets
    /// middleware redact and bound it like any other part, while a driver
    /// without a structured surface renders the same JSON text. Values that are
    /// not JSON objects are refused, as core refuses them.
    pub fn json(value: &str) -> Result<Self, String> {
        let part = DataPart {
            media_type: "application/json".into(),
            value: value.trim().to_owned(),
        };
        let content = Content {
            parts: vec![Part::Data(part)],
        };
        content.validate()?;
        Ok(content)
    }

    /// core's `Content.Validate`: at least one part, and every part valid.
    pub fn validate(&self) -> Result<(), String> {
        if self.parts.is_empty() {
            return Err("content must contain at least one part".into());
        }
        for (index, part) in self.parts.iter().enumerate() {
            part.validate()
                .map_err(|err| format!("content part {index}: {err}"))?;
        }
        Ok(())
    }

    /// Every text part, concatenated in order. Non-text parts are skipped: this
    /// answers "what did the turn say" without imposing prose structure on
    /// multi-modal content.
    pub fn text_of(&self) -> String {
        let mut out = String::new();
        for part in &self.parts {
            if let Part::Text(part) = part {
                out.push_str(&part.text);
            }
        }
        out
    }

    /// The wire form: `{"parts": [...]}`. Validates first, so a content that
    /// cannot be stored cannot be written either.
    pub fn to_wire_json(&self) -> Result<String, String> {
        self.validate()?;
        let mut out = String::from("{\"parts\":[");
        for (index, part) in self.parts.iter().enumerate() {
            if index > 0 {
                out.push(',');
            }
            out.push_str(
                &part
                    .to_wire_json()
                    .map_err(|err| format!("content part {index}: {err}"))?,
            );
        }
        out.push_str("]}");
        Ok(out)
    }

    /// core's `Content.UnmarshalJSON`: a strict object, parts in order, then the
    /// content rules.
    pub fn from_wire_json(document: &str) -> Result<Content, String> {
        // `null` decodes into core's struct as a no-op and then fails the
        // content rules, so that is the answer here too.
        let members = match json::Members::parse(document) {
            Ok(members) => members,
            Err(_) if document.trim() == "null" => {
                return Err("content must contain at least one part".into());
            }
            Err(err) => return Err(err),
        };
        if let Some(unknown) = members.first_unknown(&["parts"]) {
            return Err(format!("json: unknown field {}", text::quote(unknown)));
        }
        let mut parts = Vec::new();
        if let Some(raw) = members.raw("parts").filter(|raw| *raw != "null") {
            for (index, part) in json::raw_array(raw)?.into_iter().enumerate() {
                parts.push(
                    Part::from_wire_json(part.get())
                        .map_err(|err| format!("content part {index}: {err}"))?,
                );
            }
        }
        let content = Content { parts };
        content.validate()?;
        Ok(content)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::message::media::ImageSource;

    /// Unwraps a rule the test expects to hold; a refusal fails the test with
    /// its own sentence instead of a backtrace line.
    fn ok<T>(result: Result<T, String>, what: &str) -> T {
        match result {
            Ok(value) => value,
            Err(err) => panic!("{what}: {err}"),
        }
    }

    /// The sentence a rule the test expects to be refused answered with.
    fn refused<T>(result: Result<T, String>, what: &str) -> String {
        match result {
            Ok(_) => panic!("{what}: expected a refusal"),
            Err(err) => err,
        }
    }

    #[test]
    fn a_json_content_is_one_data_part() {
        let content = ok(Content::json(r#"  { "a": 1 }  "#), "a JSON object");
        let [Part::Data(part)] = content.parts.as_slice() else {
            panic!("one data part");
        };
        assert_eq!(part.media_type, "application/json");
        // The text is trimmed, not re-rendered: what a caller handed in is what
        // the wire carries, minus its surroundings.
        assert_eq!(part.value, r#"{ "a": 1 }"#);
        assert_eq!(
            ok(content.to_wire_json(), "a valid content"),
            r#"{"parts":[{"type":"data","media_type":"application/json","value":{"a":1}}]}"#
        );
        // A value that is not an object is refused, as core refuses it.
        assert_eq!(
            refused(Content::json("[1]"), "a value that is not an object"),
            "content part 0: data part value must be a JSON object"
        );
    }

    #[test]
    fn text_of_concatenates_only_text_parts() {
        let content = Content {
            parts: vec![
                Part::Text(TextPart::new("a")),
                Part::Image(ImagePart::new(ok(
                    ImageSource::new_url("https://x/y.png", "image/png"),
                    "a source",
                ))),
                Part::Text(TextPart::new("b")),
            ],
        };
        assert_eq!(content.text_of(), "ab");
    }

    #[test]
    fn messages_find_their_tool_parts() {
        let message = ok(
            Message::from_wire_json(
                r#"{"role":"assistant","content":{"parts":[{"type":"text","text":"a"},{"type":"tool_call","call":{"id":"c1","name":"search","arguments":{"q":"x"}}}]}}"#,
            ),
            "a valid message",
        );
        assert!(message.has_tool_calls());
        assert_eq!(message.tool_calls().len(), 1);
        assert!(message.tool_results().is_empty());

        let tool = ok(
            Message::from_wire_json(
                r#"{"role":"tool","content":{"parts":[{"type":"tool_result","result":{"call_id":"c1","content":{"parts":[{"type":"text","text":"ok"}]}}}]}}"#,
            ),
            "a valid message",
        );
        assert!(!tool.has_tool_calls());
        assert_eq!(tool.tool_results()[0].call_id, "c1");
    }

    #[test]
    fn last_by_role_scans_backwards() {
        let messages = vec![
            Message::text(Role::User, "first"),
            Message::text(Role::Assistant, "answer"),
            Message::text(Role::User, "second"),
        ];
        assert_eq!(
            last_by_role(&messages, Role::User).map(|message| message.content.text_of()),
            Some("second".to_owned())
        );
        assert!(last_by_role(&messages, Role::System).is_none());
    }

    #[test]
    fn validate_holds_the_role_rules() {
        let call = Part::ToolCall(ToolCallPart {
            call: ToolCall {
                id: "c1".into(),
                name: "search".into(),
                arguments: "{}".into(),
            },
        });
        let user = Message {
            role: Role::User,
            content: Content {
                parts: vec![call.clone()],
            },
        };
        assert_eq!(
            refused(user.validate(), "a tool call in a user turn"),
            "tool call parts require assistant role"
        );
        let assistant = Message {
            role: Role::Assistant,
            content: Content { parts: vec![call] },
        };
        assert_eq!(assistant.validate(), Ok(()));

        let empty = Message {
            role: Role::User,
            content: Content::default(),
        };
        assert_eq!(
            refused(empty.validate(), "a message with no parts"),
            "message content is required"
        );
    }
}
