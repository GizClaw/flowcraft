//! Which fields a request activates — the ledger's list of what must be reported.
//!
//! Ported from `core/inference/generate_request.go::ActiveFields` and the tables in
//! `generate_ledger.go`. This is the folding rule the plan calls one of the two places
//! where "it runs but is not equivalent" happens, so it is written as a pure function
//! of the request with the order pinned by a test.
//!
//! Two things it is *not*:
//!
//! - it is not "the fields the driver bothered to look at". A field is active because
//!   the **request carries it**, so a driver that ignores one is a driver whose report
//!   fails [`CompileReport::validate_success`](crate::inference::model::CompileReport::validate_success),
//!   not one that gets away with a shorter list;
//! - it is not per-provider. The vocabulary is the canonical request's, and a provider
//!   that cannot express a field says `Rejected`, which is the difference between "this
//!   request cannot be served here" and "this request was smaller than it looked".
//!
//! The order is the one the Go side emits, so a report's decision list is comparable
//! across the two trees: execution shape, context, input, intent, metadata.

use crate::inference::model::FieldId;
use crate::inference::request::{GenerateRequest, TextFormat, TextIntent};
use crate::proto::message as msg;

/// Every field `request` activates, for a unary execution.
///
/// The unary shape is named because it is one: `Generate` in this tree is the
/// non-streaming call, and a streaming shape activates `generate.execution.stream`
/// instead — one field, and the reason a shape is part of the ledger at all.
#[must_use]
pub fn active_fields(request: &GenerateRequest) -> Vec<FieldId> {
    let mut fields = vec![FieldId::GenerateExecutionUnary];
    if !request.messages.is_empty() {
        // The role axis is active as soon as there is a turn.
        fields.push(FieldId::GenerateContextRole);
        let present: std::collections::BTreeSet<Kind> = request
            .messages
            .iter()
            .filter_map(|message| message.content.as_ref())
            .flat_map(|content| content.parts.iter())
            .filter_map(|part| part.kind.as_ref())
            .map(kind_of)
            .collect();
        // Vocabulary order, not encounter order: the report reads the same way whatever
        // order a caller happened to put its parts in.
        for kind in Kind::ORDER {
            if present.contains(&kind) {
                fields.push(context_field(kind));
            }
        }
    }
    // The intent axis: this module serves text generation, so `intent.text` is active
    // whenever the request has an intent at all — which is always, since it is not
    // optional in the canonical type.
    fields.push(FieldId::GenerateIntentText);
    append_intent_fields(&mut fields, &request.intent);
    fields
}

fn append_intent_fields(fields: &mut Vec<FieldId>, intent: &TextIntent) {
    // The order mirrors the Go builder: response shape before the sampling knobs,
    // because that is the order a reviewer reads a report in.
    match &intent.format {
        TextFormat::Text => {}
        TextFormat::JsonObject => {
            fields.push(FieldId::GenerateIntentTextResponse);
            fields.push(FieldId::GenerateIntentTextResponseKind);
        }
        TextFormat::JsonSchema { name, schema } => {
            fields.push(FieldId::GenerateIntentTextResponse);
            fields.push(FieldId::GenerateIntentTextResponseKind);
            if !name.is_empty() {
                fields.push(FieldId::GenerateIntentTextResponseName);
            }
            if !schema.is_empty() {
                fields.push(FieldId::GenerateIntentTextResponseSchema);
            }
        }
    }
    if intent.max_output_tokens.is_some() {
        fields.push(FieldId::GenerateIntentTextMaxOutputTokens);
    }
    if !intent.tools.is_empty() {
        fields.push(FieldId::GenerateIntentTools);
    }
    if !intent.tools.is_empty() {
        // A choice only exists among tools.
        fields.push(FieldId::GenerateIntentToolChoice);
        fields.push(FieldId::GenerateIntentToolChoiceKind);
    }
    if intent.temperature.is_some() {
        fields.push(FieldId::GenerateIntentTemperature);
    }
    if let Some(effort) = &intent.reasoning_effort {
        fields.push(FieldId::GenerateIntentReasoning);
        if !effort.is_empty() {
            fields.push(FieldId::GenerateIntentReasoningEffort);
        }
    }
}

/// One content kind, as the ledger sees it.
///
/// A local copy of the vocabulary rather than a reuse of the wire's oneof, because this
/// list has an **order** and the oneof does not: `ORDER` is what makes a report
/// comparable between two requests that carry the same kinds in a different order.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
enum Kind {
    Text,
    Image,
    Audio,
    Video,
    File,
    Data,
    ToolCall,
    ToolResult,
    Reasoning,
}

impl Kind {
    /// The vocabulary order, from `message.PartKinds` on the Go side.
    const ORDER: [Self; 9] = [
        Self::Text,
        Self::Image,
        Self::Audio,
        Self::Video,
        Self::File,
        Self::Data,
        Self::ToolCall,
        Self::ToolResult,
        Self::Reasoning,
    ];
}

fn kind_of(kind: &msg::part::Kind) -> Kind {
    use msg::part::Kind as Part;
    match kind {
        Part::Text(_) => Kind::Text,
        Part::Image(_) => Kind::Image,
        Part::Audio(_) => Kind::Audio,
        Part::Video(_) => Kind::Video,
        Part::File(_) => Kind::File,
        Part::Data(_) => Kind::Data,
        Part::ToolCall(_) => Kind::ToolCall,
        Part::ToolResult(_) => Kind::ToolResult,
        Part::Reasoning(_) => Kind::Reasoning,
    }
}

const fn context_field(kind: Kind) -> FieldId {
    match kind {
        Kind::Text => FieldId::GenerateContextText,
        Kind::Image => FieldId::GenerateContextImage,
        Kind::Audio => FieldId::GenerateContextAudio,
        Kind::Video => FieldId::GenerateContextVideo,
        Kind::File => FieldId::GenerateContextFile,
        Kind::Data => FieldId::GenerateContextData,
        Kind::ToolCall => FieldId::GenerateContextToolCall,
        Kind::ToolResult => FieldId::GenerateContextToolResult,
        Kind::Reasoning => FieldId::GenerateContextReasoning,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::inference::request::ToolChoice;

    fn turn(role: msg::Role, parts: Vec<msg::part::Kind>) -> msg::Message {
        msg::Message {
            role: role as i32,
            content: Some(msg::Content {
                parts: parts
                    .into_iter()
                    .map(|kind| msg::Part { kind: Some(kind) })
                    .collect(),
            }),
        }
    }

    fn text(text: &str) -> msg::part::Kind {
        msg::part::Kind::Text(msg::TextPart {
            text: text.to_owned(),
        })
    }

    fn request(messages: Vec<msg::Message>, intent: TextIntent) -> GenerateRequest {
        GenerateRequest {
            model: "gpt-5".to_owned(),
            messages,
            intent,
        }
    }

    #[test]
    fn a_plain_text_turn_activates_the_execution_context_and_text() {
        assert_eq!(
            active_fields(&request(
                vec![turn(msg::Role::User, vec![text("hi")])],
                TextIntent::default()
            )),
            vec![
                FieldId::GenerateExecutionUnary,
                FieldId::GenerateContextRole,
                FieldId::GenerateContextText,
                FieldId::GenerateIntentText,
            ]
        );
    }

    /// The point of a vocabulary order: the report reads the same way every time, and
    /// adding a content kind cannot reorder the ones before it.
    #[test]
    fn part_kinds_activate_in_vocabulary_order_not_encounter_order() {
        let image = msg::part::Kind::Image(msg::ImagePart {
            source: Some(msg::MediaSource {
                media_type: "image/png".to_owned(),
                from: Some(msg::media_source::From::Url("https://x/y.png".to_owned())),
            }),
        });
        let fields = active_fields(&request(
            vec![turn(msg::Role::User, vec![image, text("hi")])],
            TextIntent::default(),
        ));
        assert_eq!(
            &fields[1..4],
            &[
                FieldId::GenerateContextRole,
                FieldId::GenerateContextText,
                FieldId::GenerateContextImage,
            ]
        );
    }

    #[test]
    fn the_intent_axes_activate_what_they_actually_carry() {
        let fields = active_fields(&request(
            vec![turn(msg::Role::User, vec![text("hi")])],
            TextIntent {
                max_output_tokens: Some(64),
                temperature: Some(0.2),
                tools: vec![msg::ToolDefinition {
                    name: "clock".to_owned(),
                    description: String::new(),
                    input_schema: Vec::new(),
                }],
                tool_choice: ToolChoice::Required,
                format: TextFormat::JsonSchema {
                    name: "answer".to_owned(),
                    schema: r#"{"type":"object"}"#.to_owned(),
                },
                reasoning_effort: Some("high".to_owned()),
            },
        ));
        for field in [
            FieldId::GenerateIntentTextResponse,
            FieldId::GenerateIntentTextResponseKind,
            FieldId::GenerateIntentTextResponseName,
            FieldId::GenerateIntentTextResponseSchema,
            FieldId::GenerateIntentTextMaxOutputTokens,
            FieldId::GenerateIntentTools,
            FieldId::GenerateIntentToolChoice,
            FieldId::GenerateIntentToolChoiceKind,
            FieldId::GenerateIntentTemperature,
            FieldId::GenerateIntentReasoning,
            FieldId::GenerateIntentReasoningEffort,
        ] {
            assert!(fields.contains(&field), "{field} missing from {fields:?}");
        }
        // Nothing that the request does not carry.
        assert!(!fields.contains(&FieldId::GenerateIntentTopP));
        assert!(!fields.contains(&FieldId::GenerateIntentImage));
    }

    /// An empty request still names the shape and the intent: a driver cannot report
    /// "nothing happened".
    #[test]
    fn an_empty_request_still_activates_the_shape() {
        let fields = active_fields(&request(Vec::new(), TextIntent::default()));
        assert_eq!(
            fields,
            vec![FieldId::GenerateExecutionUnary, FieldId::GenerateIntentText]
        );
    }
}
