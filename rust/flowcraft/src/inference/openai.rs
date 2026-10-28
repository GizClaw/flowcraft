//! The OpenAI adapter: the Responses API, unary generation.
//!
//! One vendor to prove the pipeline on, and one dialect of it: `api: response`. The
//! Chat Completions dialect is *not* here on purpose — it is a second lowering of the
//! same request with its own losses (Responses-only reasoning round-trips, a hosted
//! tool that only exists on one side), and a first adapter is worth nothing if it
//! spreads over two shapes before the contract has been tested once.
//!
//! Two rules from the design hold here:
//!
//! - **the compiler does no I/O.** `compile` is a pure function of the canonical
//!   request; the transport stage is the only one that talks to a provider, which is
//!   what makes a compile testable as a JSON snapshot.
//! - **`store: false` is explicit.** The Responses API retains a response server-side
//!   by default. A sandbox platform that leaves that on has made a data-residency
//!   decision by omission.

use serde_json::{Map, Value, json};

use crate::error::{Error, Result};
use crate::inference::model::{CompileReport, Decision, Disposition, FieldId, Operation};
use crate::inference::request::{
    FinishReason, GenerateRequest, GenerateResponse, TextFormat, ToolChoice,
};
use crate::inference::usage::Usage;
use crate::proto::message as msg;

/// The provider id this adapter answers to.
pub const PROVIDER: &str = "openai";
/// The dialect this adapter implements. The other one is `chat`.
pub const API_RESPONSES: &str = "response";

/// The OpenAI Responses adapter.
#[derive(Debug, Clone, Copy, Default)]
pub struct OpenAi;

impl OpenAi {
    /// A stateless adapter.
    #[must_use]
    pub fn new() -> Self {
        Self
    }

    /// Lower a canonical request into a Responses API body.
    ///
    /// Fields the canonical request does not carry are omitted rather than defaulted:
    /// a provider's default is the provider's, and second-guessing it in the adapter
    /// makes the two disagree without anybody noticing.
    pub fn compile(
        &self,
        request: &GenerateRequest,
    ) -> Result<crate::inference::request::Compiled> {
        let mut decisions = vec![
            native(FieldId::GenerateExecutionUnary),
            native(FieldId::GenerateContextRole),
        ];
        let mut input: Vec<Value> = Vec::new();
        let mut instructions: Vec<String> = Vec::new();
        let mut has_text = false;

        for message in &request.messages {
            let role = role_name(message.role)?;
            let mut content: Vec<Value> = Vec::new();
            if let Some(content_value) = &message.content {
                for part in &content_value.parts {
                    match part.kind.as_ref() {
                        Some(msg::part::Kind::Text(text)) => {
                            has_text = true;
                            content.push(json!({
                                "type": if role == "assistant" { "output_text" } else { "input_text" },
                                "text": text.text,
                            }));
                        }
                        // The first adapter lower text only. A picture is not dropped
                        // quietly: the report names the field and the reason, and the
                        // compile fails, because a prompt that lost its image is a
                        // different prompt.
                        Some(other) => {
                            let (field, reason) = unsupported_kind(other);
                            decisions.push(Decision {
                                field,
                                disposition: Disposition::Rejected,
                                reason: reason.to_owned(),
                                components: Vec::new(),
                            });
                        }
                        None => {
                            return Err(Error::validation("inference: a content part has no kind"));
                        }
                    }
                }
            }
            if content.is_empty() {
                continue;
            }
            // A system turn is the Responses API's `instructions`, not an input item.
            if role == "system" {
                instructions.push(text_of(&content));
                continue;
            }
            input.push(json!({ "type": "message", "role": role, "content": content }));
        }
        if has_text {
            decisions.push(native(FieldId::GenerateContextText));
        }

        let mut body = Map::new();
        body.insert("model".to_owned(), json!(request.model));
        body.insert("input".to_owned(), Value::Array(input));
        body.insert("store".to_owned(), json!(false));
        if !instructions.is_empty() {
            body.insert("instructions".to_owned(), json!(instructions.join("\n")));
        }
        let intent = &request.intent;
        if let Some(limit) = intent.max_output_tokens {
            body.insert("max_output_tokens".to_owned(), json!(limit));
            decisions.push(native(FieldId::GenerateIntentTextMaxOutputTokens));
        }
        if let Some(temperature) = intent.temperature {
            body.insert("temperature".to_owned(), json!(temperature));
            decisions.push(native(FieldId::GenerateIntentTemperature));
        }
        if !intent.tools.is_empty() {
            let tools: Vec<Value> = intent
                .tools
                .iter()
                .map(|tool| {
                    json!({
                        "type": "function",
                        "name": tool.name,
                        "description": tool.description,
                        // The schema travels as JSON, because it is JSON: parsing and
                        // re-serializing it here would only lose formatting the
                        // provider's own validator might care about.
                        "parameters": serde_json::from_slice::<Value>(&tool.input_schema)
                            .unwrap_or_else(|_| json!({"type": "object"})),
                        "strict": false,
                    })
                })
                .collect();
            body.insert("tools".to_owned(), Value::Array(tools));
            decisions.push(native(FieldId::GenerateIntentTools));
            body.insert(
                "tool_choice".to_owned(),
                json!(match intent.tool_choice {
                    ToolChoice::Auto => "auto",
                    ToolChoice::Required => "required",
                    ToolChoice::None => "none",
                }),
            );
            decisions.push(native(FieldId::GenerateIntentToolChoice));
        }
        if let Some(effort) = &intent.reasoning_effort {
            body.insert("reasoning".to_owned(), json!({ "effort": effort }));
            decisions.push(native(FieldId::GenerateIntentReasoningEffort));
        }
        match &intent.format {
            TextFormat::Text => {}
            TextFormat::JsonObject => {
                body.insert(
                    "text".to_owned(),
                    json!({"format": {"type": "json_object"}}),
                );
                decisions.push(native(FieldId::GenerateIntentTextResponse));
            }
            TextFormat::JsonSchema { name, schema } => {
                let schema = serde_json::from_str::<Value>(schema).map_err(|error| {
                    Error::validation(format!(
                        "inference: the response schema is not JSON: {error}"
                    ))
                })?;
                body.insert(
                    "text".to_owned(),
                    json!({"format": {
                        "type": "json_schema",
                        "name": name,
                        "schema": schema,
                        "strict": true,
                    }}),
                );
                decisions.push(native(FieldId::GenerateIntentTextResponse));
                decisions.push(native(FieldId::GenerateIntentTextResponseSchema));
            }
        }

        let report = CompileReport::new(Operation::Generate, decisions);
        // A rejection means the request cannot be honoured as written; the report is
        // still returned inside the error path by the caller, so the *reason* travels
        // even though nothing will be sent.
        if let Some(rejected) = report
            .decisions
            .iter()
            .find(|decision| decision.disposition == Disposition::Rejected)
        {
            return Err(Error::validation(format!(
                "inference: this provider cannot express {} ({})",
                rejected.field, rejected.reason
            )));
        }
        Ok(crate::inference::request::Compiled {
            body: Value::Object(body),
            report,
        })
    }

    /// Decode one Responses API body into a canonical response.
    pub fn decode(&self, raw: &Value) -> Result<GenerateResponse> {
        let output = raw
            .get("output")
            .and_then(Value::as_array)
            .ok_or_else(|| Error::internal("inference: the response has no output array"))?;
        let mut parts: Vec<msg::Part> = Vec::new();
        let mut wants_tool = false;
        for item in output {
            match item.get("type").and_then(Value::as_str) {
                Some("message") => {
                    let content = item
                        .get("content")
                        .and_then(Value::as_array)
                        .map(Vec::as_slice)
                        .unwrap_or_default();
                    for piece in content {
                        // `output_text` is the answer; `refusal` is prose the provider
                        // produced instead of one, and it is more honest to hand it back
                        // as text than to drop it.
                        let is_text = matches!(
                            piece.get("type").and_then(Value::as_str),
                            Some("output_text" | "refusal")
                        );
                        if is_text && let Some(text) = piece.get("text").and_then(Value::as_str) {
                            parts.push(text_part(text));
                        }
                    }
                }
                Some("function_call") => wants_tool = true,
                // Reasoning items, web-search results and anything the provider adds
                // later are not canonical content: the caller asked for an answer, and
                // this adapter's job is not to invent parts for new item types.
                _ => {}
            }
        }
        let finish = if wants_tool {
            FinishReason::ToolCalls
        } else if raw
            .get("incomplete_details")
            .and_then(|details| details.get("reason"))
            .and_then(Value::as_str)
            == Some("max_output_tokens")
        {
            FinishReason::Length
        } else {
            FinishReason::Stop
        };
        Ok(GenerateResponse {
            parts,
            finish,
            usage: usage_of(raw.get("usage")),
        })
    }
}

fn text_part(text: &str) -> msg::Part {
    msg::Part {
        kind: Some(msg::part::Kind::Text(msg::TextPart {
            text: text.to_owned(),
        })),
    }
}

fn native(field: FieldId) -> Decision {
    Decision {
        field,
        disposition: Disposition::Native,
        reason: String::new(),
        components: Vec::new(),
    }
}

fn role_name(role: i32) -> Result<&'static str> {
    match msg::Role::try_from(role) {
        Ok(msg::Role::System) => Ok("system"),
        Ok(msg::Role::User) => Ok("user"),
        Ok(msg::Role::Assistant) => Ok("assistant"),
        Ok(msg::Role::Tool) => Ok("user"),
        Ok(msg::Role::Unspecified) | Err(_) => Err(Error::validation(format!(
            "inference: {role} is not a message role"
        ))),
    }
}

fn unsupported_kind(kind: &msg::part::Kind) -> (FieldId, &'static str) {
    use msg::part::Kind;
    match kind {
        Kind::Image(_) => (
            FieldId::GenerateContextImage,
            "this adapter lowers text only; vision arrives with its own conformance suite",
        ),
        Kind::Audio(_) => (FieldId::GenerateContextAudio, "no audio channel yet"),
        Kind::Video(_) => (FieldId::GenerateContextVideo, "no video channel yet"),
        Kind::File(_) => (FieldId::GenerateContextFile, "no file channel yet"),
        Kind::Data(_) => (FieldId::GenerateContextData, "no data-part channel yet"),
        Kind::ToolCall(_) => (
            FieldId::GenerateContextToolCall,
            "caller-supplied tool calls are not lowered yet",
        ),
        Kind::ToolResult(_) => (
            FieldId::GenerateContextToolResult,
            "caller-supplied tool results are not lowered yet",
        ),
        Kind::Reasoning(_) => (
            FieldId::GenerateContextReasoning,
            "reasoning round-trips are not lowered yet",
        ),
        Kind::Text(_) => (FieldId::GenerateContextText, "text is lowered"),
    }
}

fn text_of(content: &[Value]) -> String {
    content
        .iter()
        .filter_map(|piece| piece.get("text").and_then(Value::as_str))
        .collect::<Vec<_>>()
        .join("\n")
}

fn usage_of(usage: Option<&Value>) -> Usage {
    let Some(usage) = usage else {
        return Usage::default();
    };
    Usage {
        input_tokens: count(usage, "input_tokens"),
        output_tokens: count(usage, "output_tokens"),
        total_tokens: count(usage, "total_tokens"),
        cache_read_tokens: nested_count(usage, "input_tokens_details", "cached_tokens"),
        reasoning_tokens: nested_count(usage, "output_tokens_details", "reasoning_tokens"),
    }
}

fn count(value: &Value, key: &str) -> u64 {
    value.get(key).and_then(Value::as_u64).unwrap_or(0)
}

/// A nested counter that stays `None` when the provider did not report it — the rule
/// `Usage::add` depends on.
fn nested_count(value: &Value, parent: &str, key: &str) -> Option<u64> {
    value
        .get(parent)
        .and_then(|nested| nested.get(key))
        .and_then(Value::as_u64)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::inference::request::{GenerateRequest, TextIntent};

    fn turn(role: msg::Role, text: &str) -> msg::Message {
        msg::Message {
            role: role as i32,
            content: Some(msg::Content {
                parts: vec![text_part(text)],
            }),
        }
    }

    fn request() -> GenerateRequest {
        GenerateRequest {
            model: "gpt-5".to_owned(),
            messages: vec![
                turn(msg::Role::System, "be brief"),
                turn(msg::Role::User, "hi"),
            ],
            intent: TextIntent {
                max_output_tokens: Some(64),
                temperature: Some(0.2),
                ..TextIntent::default()
            },
        }
    }

    /// The body is the contract with the provider, so it is pinned as JSON rather than
    /// checked field by field: a `serde` attribute that renames something shows up here
    /// and nowhere else.
    #[test]
    fn a_text_request_lowers_to_the_responses_body() {
        let compiled = OpenAi::new().compile(&request()).expect("compile");
        assert_eq!(
            compiled.body,
            json!({
                "model": "gpt-5",
                "input": [
                    {"type": "message", "role": "user", "content": [
                        {"type": "input_text", "text": "hi"},
                    ]},
                ],
                "instructions": "be brief",
                // Never defaulted: the Responses API retains server-side otherwise.
                "store": false,
                "max_output_tokens": 64,
                "temperature": 0.2,
            })
        );
        // And the report proves it covered what the request carried.
        compiled
            .report
            .validate_success(
                Operation::Generate,
                &[
                    FieldId::GenerateExecutionUnary,
                    FieldId::GenerateContextRole,
                    FieldId::GenerateContextText,
                    FieldId::GenerateIntentTextMaxOutputTokens,
                    FieldId::GenerateIntentTemperature,
                ],
            )
            .expect("a complete report");
    }

    #[test]
    fn an_assistant_turn_is_output_text_and_tools_are_functions() {
        let mut request = request();
        request.messages.push(turn(msg::Role::Assistant, "hello"));
        request.intent.tools = vec![msg::ToolDefinition {
            name: "clock".to_owned(),
            description: "what time is it".to_owned(),
            input_schema: br#"{"type":"object","properties":{}}"#.to_vec(),
        }];
        request.intent.tool_choice = ToolChoice::Required;
        request.intent.format = TextFormat::JsonObject;
        let body = OpenAi::new().compile(&request).expect("compile").body;
        assert_eq!(body["input"][1]["content"][0]["type"], "output_text");
        assert_eq!(body["tools"][0]["type"], "function");
        assert_eq!(body["tools"][0]["name"], "clock");
        assert_eq!(body["tools"][0]["parameters"]["type"], "object");
        assert_eq!(body["tool_choice"], "required");
        assert_eq!(body["text"]["format"]["type"], "json_object");
    }

    /// A prompt that lost its picture is a different prompt, so the compile fails and
    /// names the field — the report is how that reaches an audit trail.
    #[test]
    fn a_picture_is_rejected_with_the_field_named() {
        let mut request = request();
        request.messages[1].content = Some(msg::Content {
            parts: vec![msg::Part {
                kind: Some(msg::part::Kind::Image(msg::ImagePart {
                    source: Some(msg::MediaSource {
                        media_type: "image/png".to_owned(),
                        from: Some(msg::media_source::From::Url("https://x/y.png".to_owned())),
                    }),
                })),
            }],
        });
        let error = OpenAi::new().compile(&request).expect_err("refused");
        assert_eq!(error.kind(), crate::error::ErrorKind::Validation);
        assert!(
            error
                .to_string()
                .contains("generate.context.*.content.parts.image"),
            "{error}"
        );
    }

    #[test]
    fn a_response_decodes_to_parts_and_usage() {
        let raw = json!({
            "status": "completed",
            "output": [
                {"type": "reasoning", "summary": []},
                {"type": "message", "role": "assistant", "content": [
                    {"type": "output_text", "text": "hello there"},
                ]},
            ],
            "usage": {
                "input_tokens": 11,
                "output_tokens": 3,
                "total_tokens": 14,
                "input_tokens_details": {"cached_tokens": 8},
                "output_tokens_details": {"reasoning_tokens": 2},
            },
        });
        let response = OpenAi::new().decode(&raw).expect("decode");
        assert_eq!(response.finish, FinishReason::Stop);
        assert_eq!(response.parts.len(), 1);
        // The reasoning item is not canonical content: it is not handed back as prose.
        assert_eq!(response.usage.input_tokens, 11);
        assert_eq!(response.usage.cache_read_tokens, Some(8));
        assert_eq!(response.usage.reasoning_tokens, Some(2));
    }

    #[test]
    fn a_cut_off_response_says_so() {
        let raw = json!({
            "status": "incomplete",
            "incomplete_details": {"reason": "max_output_tokens"},
            "output": [{"type": "message", "content": [{"type": "output_text", "text": "half"}]}],
        });
        assert_eq!(
            OpenAi::new().decode(&raw).expect("decode").finish,
            FinishReason::Length
        );

        let tool = json!({
            "output": [{"type": "function_call", "name": "clock", "arguments": "{}"}],
        });
        assert_eq!(
            OpenAi::new().decode(&tool).expect("decode").finish,
            FinishReason::ToolCalls
        );
    }

    /// A provider that reports no usage must not be reported as having used zero: the
    /// `None`s stay `None` so a budget cannot silently charge for a silence.
    #[test]
    fn a_response_without_usage_reports_nothing() {
        let raw = json!({"output": []});
        let response = OpenAi::new().decode(&raw).expect("decode");
        assert!(response.usage.is_unreported());
        assert_eq!(response.usage.cache_read_tokens, None);
    }
}
