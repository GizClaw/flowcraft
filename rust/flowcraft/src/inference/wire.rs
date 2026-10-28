//! The wire forms of the canonical vocabulary.
//!
//! `inference/v1` reuses `flowcraft.message.v1` for content and adds only the intent,
//! evidence and usage layers — so most of this module is a translation of *types*, plus
//! one genuine two-representation problem:
//!
//! **`FieldId` is an enum on the wire and a dotted string everywhere else.** The enum is
//! the type-safe form a compiler matches on; the string
//! (`generate.context.*.content.parts.text`) is what appears in checkpoint metadata,
//! telemetry and compiled reports, and it is spelled exactly as the Go table spells it.
//! Both directions are exhaustive, so adding a field without a wire value is a compile
//! error rather than a field that cannot be reported.

use crate::error::{Error, Result};
use crate::inference::ledger::Attempt;
use crate::inference::model::{CompileReport, Decision, Disposition, FieldId, Operation};
use crate::inference::request::GenerateEvent;
use crate::inference::request::{
    FinishReason, GenerateRequest, GenerateResponse, TextFormat, TextIntent, ToolChoice,
};
use crate::inference::usage::{Money, Usage};
use crate::proto::inference as pb;
use crate::proto::message as msg;

/// Decode an operation string.
pub fn operation_from_wire(raw: &str) -> Result<Operation> {
    Operation::parse(raw)
}

/// Encode a request.
///
/// A canonical request is already resolved to a model, so the target is the exact form;
/// a `tier` is what a *client* sends and routing resolves before this point.
#[must_use]
pub fn request_to_proto(request: &GenerateRequest) -> pb::GenerateRequest {
    pb::GenerateRequest {
        target: Some(pb::Target {
            choose: Some(pb::target::Choose::Model(pb::ModelRef {
                id: Some(pb::ModelId {
                    provider: String::new(),
                    name: request.model.clone(),
                }),
            })),
        }),
        messages: request.messages.clone(),
        intent: Some(intent_to_proto(&request.intent)),
    }
}

/// Decode a request.
///
/// A request that names a tier or a hint cannot be decoded: routing owns the transition
/// from one to a model, so seeing one here means the peer skipped that layer. That is a
/// `Validation` error, not a guess.
pub fn request_from_proto(request: pb::GenerateRequest) -> Result<GenerateRequest> {
    let choose = request
        .target
        .and_then(|target| target.choose)
        .ok_or_else(|| Error::validation("inference: a request must name a target"))?;
    let model = match choose {
        pb::target::Choose::Model(model) => model
            .id
            .map(|id| id.name)
            .filter(|name| !name.is_empty())
            .ok_or_else(|| Error::validation("inference: the target names no model"))?,
        pb::target::Choose::Tier(tier) => {
            return Err(Error::validation(format!(
                "inference: target tier {tier:?} reached the executor; routing resolves a tier \
                 before it does"
            )));
        }
        pb::target::Choose::ModelHint(hint) => {
            return Err(Error::validation(format!(
                "inference: target hint {hint:?} reached the executor; only a resolved model does"
            )));
        }
    };
    Ok(GenerateRequest {
        model,
        messages: request.messages,
        intent: request.intent.map(intent_from_proto).unwrap_or_default(),
    })
}

/// Encode the text intent.
#[must_use]
pub fn intent_to_proto(intent: &TextIntent) -> pb::TextIntent {
    pb::TextIntent {
        max_output_tokens: intent.max_output_tokens,
        temperature: intent.temperature,
        tools: intent.tools.clone(),
        tool_choice: Some(pb::ToolChoice {
            kind: match intent.tool_choice {
                ToolChoice::Auto => pb::tool_choice::Kind::Auto as i32,
                ToolChoice::Required => pb::tool_choice::Kind::Required as i32,
                ToolChoice::None => pb::tool_choice::Kind::None as i32,
            },
            name: String::new(),
        }),
        format: Some(format_to_proto(&intent.format)),
        reasoning_effort: intent.reasoning_effort.clone().unwrap_or_default(),
    }
}

/// Decode the text intent.
#[must_use]
pub fn intent_from_proto(intent: pb::TextIntent) -> TextIntent {
    TextIntent {
        max_output_tokens: intent.max_output_tokens,
        temperature: intent.temperature,
        tools: intent.tools,
        tool_choice: intent
            .tool_choice
            .map(
                |choice| match pb::tool_choice::Kind::try_from(choice.kind) {
                    Ok(pb::tool_choice::Kind::Required) => ToolChoice::Required,
                    Ok(pb::tool_choice::Kind::None) => ToolChoice::None,
                    // Unspecified, and an unknown value from a newer peer, both read as the
                    // default rather than as an error: a choice this build does not know is
                    // still the provider's decision to make.
                    _ => ToolChoice::Auto,
                },
            )
            .unwrap_or_default(),
        format: intent.format.map(format_from_proto).unwrap_or_default(),
        reasoning_effort: Some(intent.reasoning_effort).filter(|effort| !effort.is_empty()),
    }
}

fn format_to_proto(format: &TextFormat) -> pb::ResponseFormat {
    match format {
        TextFormat::Text => pb::ResponseFormat {
            kind: pb::response_format::Kind::Text as i32,
            name: String::new(),
            schema: Vec::new(),
        },
        TextFormat::JsonObject => pb::ResponseFormat {
            kind: pb::response_format::Kind::JsonObject as i32,
            name: String::new(),
            schema: Vec::new(),
        },
        TextFormat::JsonSchema { name, schema } => pb::ResponseFormat {
            kind: pb::response_format::Kind::JsonSchema as i32,
            name: name.clone(),
            schema: schema.clone().into_bytes(),
        },
    }
}

fn format_from_proto(format: pb::ResponseFormat) -> TextFormat {
    match pb::response_format::Kind::try_from(format.kind) {
        Ok(pb::response_format::Kind::JsonObject) => TextFormat::JsonObject,
        Ok(pb::response_format::Kind::JsonSchema) => TextFormat::JsonSchema {
            name: format.name,
            schema: String::from_utf8_lossy(&format.schema).into_owned(),
        },
        _ => TextFormat::Text,
    }
}

/// Encode a compile report.
#[must_use]
pub fn report_to_proto(report: &CompileReport) -> pb::CompileReport {
    pb::CompileReport {
        operation: report.operation.as_str().to_owned(),
        decisions: report
            .decisions
            .iter()
            .map(|decision| pb::CompileDecision {
                field: field_to_proto(decision.field) as i32,
                disposition: disposition_to_proto(decision.disposition) as i32,
                reason: decision.reason.clone(),
                components: decision
                    .components
                    .iter()
                    .map(|note| pb::ComponentNote {
                        kind: note.kind.clone(),
                        disposition: disposition_to_proto(note.disposition) as i32,
                        index: u64::try_from(note.index).unwrap_or(u64::MAX),
                        reason: note.reason.clone(),
                    })
                    .collect(),
            })
            .collect(),
    }
}

/// Decode a compile report.
pub fn report_from_proto(report: pb::CompileReport) -> Result<CompileReport> {
    let mut decisions = Vec::with_capacity(report.decisions.len());
    for decision in report.decisions {
        let mut components = Vec::with_capacity(decision.components.len());
        for note in decision.components {
            components.push(crate::inference::model::ComponentNote {
                kind: note.kind,
                disposition: disposition_from_proto(note.disposition)?,
                index: usize::try_from(note.index).unwrap_or(usize::MAX),
                reason: note.reason,
            });
        }
        decisions.push(Decision {
            field: field_from_proto(decision.field)?,
            disposition: disposition_from_proto(decision.disposition)?,
            reason: decision.reason,
            components,
        });
    }
    Ok(CompileReport::new(
        operation_from_wire(&report.operation)?,
        decisions,
    ))
}

/// Encode a generation response, with the evidence for it.
#[must_use]
pub fn response_to_proto(
    response: &GenerateResponse,
    report: &CompileReport,
) -> pb::GenerateResponse {
    pb::GenerateResponse {
        parts: response.parts.clone(),
        finish: finish_to_proto(response.finish) as i32,
        usage: Some(usage_to_proto(&response.usage)),
        report: Some(report_to_proto(report)),
    }
}

/// Decode a generation response. The report comes back with it: a response without its
/// evidence is exactly what the design refuses to hand over.
pub fn response_from_proto(
    response: pb::GenerateResponse,
) -> Result<(GenerateResponse, CompileReport)> {
    let report = report_from_proto(response.report.unwrap_or_default())?;
    Ok((
        GenerateResponse {
            parts: response.parts,
            finish: finish_from_proto(response.finish),
            usage: response.usage.map(usage_from_proto).unwrap_or_default(),
        },
        report,
    ))
}

/// Encode usage.
#[must_use]
pub fn usage_to_proto(usage: &Usage) -> pb::Usage {
    pb::Usage {
        input_tokens: usage.input_tokens,
        output_tokens: usage.output_tokens,
        total_tokens: usage.total_tokens,
        cache_read_tokens: usage.cache_read_tokens,
        reasoning_tokens: usage.reasoning_tokens,
    }
}

/// Decode usage.
#[must_use]
pub fn usage_from_proto(usage: pb::Usage) -> Usage {
    Usage {
        input_tokens: usage.input_tokens,
        output_tokens: usage.output_tokens,
        total_tokens: usage.total_tokens,
        cache_read_tokens: usage.cache_read_tokens,
        reasoning_tokens: usage.reasoning_tokens,
    }
}

/// Encode money.
#[must_use]
pub fn money_to_proto(money: &Money) -> pb::Money {
    pb::Money {
        currency: money.currency.clone(),
        units: money.units,
        scale: u32::from(money.scale),
    }
}

/// Decode money.
#[must_use]
pub fn money_from_proto(money: pb::Money) -> Money {
    Money {
        currency: money.currency,
        units: money.units,
        scale: u8::try_from(money.scale).unwrap_or(u8::MAX),
    }
}

/// Encode one streaming event.
///
/// `report` belongs to the stream rather than to an event — the compile happens before the
/// first token — so it is an argument here and only `Finish` puts it on the wire, inside
/// the response, exactly as a unary answer does.
#[must_use]
pub fn event_to_proto(event: &GenerateEvent, report: Option<&CompileReport>) -> pb::GenerateEvent {
    let kind = match event {
        GenerateEvent::Delta(text) => pb::generate_event::Kind::Delta(msg::Delta {
            kind: Some(msg::delta::Kind::Text(text.clone())),
        }),
        GenerateEvent::Attempt(attempt) => {
            pb::generate_event::Kind::Attempt(attempt_to_proto(attempt))
        }
        GenerateEvent::Finish(response) => pb::generate_event::Kind::Finish(pb::Finish {
            response: Some(response_to_proto(
                response,
                report.unwrap_or(&CompileReport::new(Operation::Generate, Vec::new())),
            )),
        }),
    };
    pb::GenerateEvent { kind: Some(kind) }
}

/// Decode one streaming event.
///
/// `None` for an event this adapter does not produce as content: `Started` is the server
/// opening the stream (a client reads it for the response id and nothing else), and a
/// reasoning or tool-call delta is not text — inventing a text event for one would corrupt
/// the assembled answer.
pub fn event_from_proto(event: pb::GenerateEvent) -> Result<Option<GenerateEvent>> {
    let Some(kind) = event.kind else {
        return Ok(None);
    };
    Ok(match kind {
        pb::generate_event::Kind::Delta(delta) => match delta.kind {
            Some(msg::delta::Kind::Text(text)) => Some(GenerateEvent::Delta(text)),
            _ => None,
        },
        pb::generate_event::Kind::Attempt(attempt) => {
            Some(GenerateEvent::Attempt(attempt_from_proto(attempt)))
        }
        pb::generate_event::Kind::Finish(finish) => {
            let (response, _report) = response_from_proto(finish.response.unwrap_or_default())?;
            Some(GenerateEvent::Finish(response))
        }
        pb::generate_event::Kind::Started(_) => None,
    })
}

fn attempt_to_proto(attempt: &Attempt) -> pb::Attempt {
    pb::Attempt {
        target: attempt.target.clone(),
        phase: phase_wire(attempt.phase).to_owned(),
        trigger: trigger_wire(attempt.trigger).to_owned(),
        outcome: outcome_wire(attempt.outcome).to_owned(),
        error_kind: attempt.error_kind.clone().unwrap_or_default(),
        observable_output: attempt.observable_output,
        number: attempt.number,
        backoff_millis: attempt.backoff_millis,
    }
}

fn attempt_from_proto(attempt: pb::Attempt) -> Attempt {
    use crate::inference::ledger::{Outcome, Phase, Trigger};
    Attempt {
        target: attempt.target,
        phase: match attempt.phase.as_str() {
            "preflight" => Phase::Preflight,
            "open" => Phase::Open,
            _ => Phase::Execute,
        },
        trigger: match attempt.trigger.as_str() {
            "retry" => Trigger::Retry,
            "fallback" => Trigger::Fallback,
            _ => Trigger::Selection,
        },
        outcome: match attempt.outcome.as_str() {
            "failed" => Outcome::Failed,
            "opened" => Outcome::Opened,
            "skipped" => Outcome::Skipped,
            _ => Outcome::Succeeded,
        },
        error_kind: Some(attempt.error_kind).filter(|kind| !kind.is_empty()),
        observable_output: attempt.observable_output,
        number: attempt.number,
        backoff_millis: attempt.backoff_millis,
    }
}

fn phase_wire(phase: crate::inference::ledger::Phase) -> &'static str {
    use crate::inference::ledger::Phase;
    match phase {
        Phase::Preflight => "preflight",
        Phase::Execute => "execute",
        Phase::Open => "open",
    }
}

fn trigger_wire(trigger: crate::inference::ledger::Trigger) -> &'static str {
    use crate::inference::ledger::Trigger;
    match trigger {
        Trigger::Selection => "selection",
        Trigger::Retry => "retry",
        Trigger::Fallback => "fallback",
    }
}

fn outcome_wire(outcome: crate::inference::ledger::Outcome) -> &'static str {
    use crate::inference::ledger::Outcome;
    match outcome {
        Outcome::Succeeded => "succeeded",
        Outcome::Failed => "failed",
        Outcome::Opened => "opened",
        Outcome::Skipped => "skipped",
    }
}

fn finish_to_proto(finish: FinishReason) -> pb::FinishReason {
    match finish {
        FinishReason::Stop => pb::FinishReason::Stop,
        FinishReason::Length => pb::FinishReason::Length,
        FinishReason::ToolCalls => pb::FinishReason::ToolCalls,
        FinishReason::Other => pb::FinishReason::Other,
    }
}

fn finish_from_proto(finish: i32) -> FinishReason {
    match pb::FinishReason::try_from(finish) {
        Ok(pb::FinishReason::Length) => FinishReason::Length,
        Ok(pb::FinishReason::ToolCalls) => FinishReason::ToolCalls,
        Ok(pb::FinishReason::Other) => FinishReason::Other,
        // Unspecified and unknown both mean "the provider did not say", and `Stop` is the
        // only terminal reading that does not invent a failure.
        _ => FinishReason::Stop,
    }
}

fn disposition_to_proto(disposition: Disposition) -> pb::Disposition {
    match disposition {
        Disposition::Native => pb::Disposition::Native,
        Disposition::Rejected => pb::Disposition::Rejected,
        Disposition::Dropped => pb::Disposition::Dropped,
    }
}

fn disposition_from_proto(disposition: i32) -> Result<Disposition> {
    match pb::Disposition::try_from(disposition) {
        Ok(pb::Disposition::Native) => Ok(Disposition::Native),
        Ok(pb::Disposition::Rejected) => Ok(Disposition::Rejected),
        Ok(pb::Disposition::Dropped) => Ok(Disposition::Dropped),
        _ => Err(Error::validation(
            "inference: a compile decision has no disposition",
        )),
    }
}

/// The enum for a field. Exhaustive, so a new field cannot forget its wire value.
fn field_to_proto(field: FieldId) -> pb::FieldId {
    match field {
        FieldId::DecisionInputJSON => pb::FieldId::DecisionInputJson,
        FieldId::DecisionInputPartImage => pb::FieldId::DecisionInputPartImage,
        FieldId::DecisionInputPartText => pb::FieldId::DecisionInputPartText,
        FieldId::DecisionInputText => pb::FieldId::DecisionInputText,
        FieldId::DecisionQuestionChoices => pb::FieldId::DecisionQuestionChoices,
        FieldId::DecisionQuestionInstructions => pb::FieldId::DecisionQuestionInstructions,
        FieldId::DecisionQuestionKind => pb::FieldId::DecisionQuestionKind,
        FieldId::DecisionQuestionLevels => pb::FieldId::DecisionQuestionLevels,
        FieldId::DecisionQuestionName => pb::FieldId::DecisionQuestionName,
        FieldId::DecisionRequestMetadata => pb::FieldId::DecisionRequestMetadata,
        FieldId::EmbedDimensions => pb::FieldId::EmbedDimensions,
        FieldId::EmbedItems => pb::FieldId::EmbedItems,
        FieldId::EmbedItemAudio => pb::FieldId::EmbedItemAudio,
        FieldId::EmbedItemData => pb::FieldId::EmbedItemData,
        FieldId::EmbedItemFile => pb::FieldId::EmbedItemFile,
        FieldId::EmbedItemImage => pb::FieldId::EmbedItemImage,
        FieldId::EmbedItemMultiPart => pb::FieldId::EmbedItemMultiPart,
        FieldId::EmbedItemText => pb::FieldId::EmbedItemText,
        FieldId::EmbedItemToolCall => pb::FieldId::EmbedItemToolCall,
        FieldId::EmbedItemToolResult => pb::FieldId::EmbedItemToolResult,
        FieldId::EmbedItemVideo => pb::FieldId::EmbedItemVideo,
        FieldId::GenerateContextAudio => pb::FieldId::GenerateContextAudio,
        FieldId::GenerateContextData => pb::FieldId::GenerateContextData,
        FieldId::GenerateContextFile => pb::FieldId::GenerateContextFile,
        FieldId::GenerateContextImage => pb::FieldId::GenerateContextImage,
        FieldId::GenerateContextReasoning => pb::FieldId::GenerateContextReasoning,
        FieldId::GenerateContextText => pb::FieldId::GenerateContextText,
        FieldId::GenerateContextToolCall => pb::FieldId::GenerateContextToolCall,
        FieldId::GenerateContextToolResult => pb::FieldId::GenerateContextToolResult,
        FieldId::GenerateContextVideo => pb::FieldId::GenerateContextVideo,
        FieldId::GenerateContextRole => pb::FieldId::GenerateContextRole,
        FieldId::GenerateExecutionStream => pb::FieldId::GenerateExecutionStream,
        FieldId::GenerateExecutionUnary => pb::FieldId::GenerateExecutionUnary,
        FieldId::GenerateIntentAudio => pb::FieldId::GenerateIntentAudio,
        FieldId::GenerateIntentAudioCount => pb::FieldId::GenerateIntentAudioCount,
        FieldId::GenerateIntentAudioFormat => pb::FieldId::GenerateIntentAudioFormat,
        FieldId::GenerateIntentAudioFormatChannels => {
            pb::FieldId::GenerateIntentAudioFormatChannels
        }
        FieldId::GenerateIntentAudioFormatEncoding => {
            pb::FieldId::GenerateIntentAudioFormatEncoding
        }
        FieldId::GenerateIntentAudioFormatSampleRate => {
            pb::FieldId::GenerateIntentAudioFormatSampleRate
        }
        FieldId::GenerateIntentAudioSpeed => pb::FieldId::GenerateIntentAudioSpeed,
        FieldId::GenerateIntentAudioVoice => pb::FieldId::GenerateIntentAudioVoice,
        FieldId::GenerateIntentAudioVoiceID => pb::FieldId::GenerateIntentAudioVoiceId,
        FieldId::GenerateIntentAudioVoiceLanguage => pb::FieldId::GenerateIntentAudioVoiceLanguage,
        FieldId::GenerateIntentImage => pb::FieldId::GenerateIntentImage,
        FieldId::GenerateIntentImageCount => pb::FieldId::GenerateIntentImageCount,
        FieldId::GenerateIntentImageDelivery => pb::FieldId::GenerateIntentImageDelivery,
        FieldId::GenerateIntentImageOutputFormat => pb::FieldId::GenerateIntentImageOutputFormat,
        FieldId::GenerateIntentImageQuality => pb::FieldId::GenerateIntentImageQuality,
        FieldId::GenerateIntentImageSeed => pb::FieldId::GenerateIntentImageSeed,
        FieldId::GenerateIntentImageSize => pb::FieldId::GenerateIntentImageSize,
        FieldId::GenerateIntentImageSizeHeight => pb::FieldId::GenerateIntentImageSizeHeight,
        FieldId::GenerateIntentImageSizeWidth => pb::FieldId::GenerateIntentImageSizeWidth,
        FieldId::GenerateIntentText => pb::FieldId::GenerateIntentText,
        FieldId::GenerateIntentTextMaxOutputTokens => {
            pb::FieldId::GenerateIntentTextMaxOutputTokens
        }
        FieldId::GenerateIntentReasoning => pb::FieldId::GenerateIntentReasoning,
        FieldId::GenerateIntentReasoningEffort => pb::FieldId::GenerateIntentReasoningEffort,
        FieldId::GenerateIntentReasoningEnabled => pb::FieldId::GenerateIntentReasoningEnabled,
        FieldId::GenerateIntentTextResponse => pb::FieldId::GenerateIntentTextResponse,
        FieldId::GenerateIntentTextResponseKind => pb::FieldId::GenerateIntentTextResponseKind,
        FieldId::GenerateIntentTextResponseName => pb::FieldId::GenerateIntentTextResponseName,
        FieldId::GenerateIntentTextResponseSchema => pb::FieldId::GenerateIntentTextResponseSchema,
        FieldId::GenerateIntentTemperature => pb::FieldId::GenerateIntentTemperature,
        FieldId::GenerateIntentToolChoice => pb::FieldId::GenerateIntentToolChoice,
        FieldId::GenerateIntentToolChoiceKind => pb::FieldId::GenerateIntentToolChoiceKind,
        FieldId::GenerateIntentToolChoiceName => pb::FieldId::GenerateIntentToolChoiceName,
        FieldId::GenerateIntentTools => pb::FieldId::GenerateIntentTools,
        FieldId::GenerateIntentTopP => pb::FieldId::GenerateIntentTopP,
        FieldId::GenerateIntentVideo => pb::FieldId::GenerateIntentVideo,
        FieldId::GenerateIntentVideoAspectRatio => pb::FieldId::GenerateIntentVideoAspectRatio,
        FieldId::GenerateIntentVideoDuration => pb::FieldId::GenerateIntentVideoDuration,
        FieldId::GenerateIntentVideoResolution => pb::FieldId::GenerateIntentVideoResolution,
        FieldId::GenerateIntentVideoSeed => pb::FieldId::GenerateIntentVideoSeed,
        FieldId::GenerateIntentVideoWatermark => pb::FieldId::GenerateIntentVideoWatermark,
        FieldId::GenerateInputAudio => pb::FieldId::GenerateInputAudio,
        FieldId::GenerateInputData => pb::FieldId::GenerateInputData,
        FieldId::GenerateInputFile => pb::FieldId::GenerateInputFile,
        FieldId::GenerateInputImage => pb::FieldId::GenerateInputImage,
        FieldId::GenerateInputReasoning => pb::FieldId::GenerateInputReasoning,
        FieldId::GenerateInputText => pb::FieldId::GenerateInputText,
        FieldId::GenerateInputToolCall => pb::FieldId::GenerateInputToolCall,
        FieldId::GenerateInputToolResult => pb::FieldId::GenerateInputToolResult,
        FieldId::GenerateInputVideo => pb::FieldId::GenerateInputVideo,
        FieldId::GenerateInputRole => pb::FieldId::GenerateInputRole,
        FieldId::GenerateRequestMetadata => pb::FieldId::GenerateRequestMetadata,
        FieldId::TranscriptionAudio => pb::FieldId::TranscriptionAudio,
        FieldId::TranscriptionLanguage => pb::FieldId::TranscriptionLanguage,
        FieldId::TranscriptionPrompt => pb::FieldId::TranscriptionPrompt,
        FieldId::TranscriptionInputFormat => pb::FieldId::TranscriptionInputFormat,
        FieldId::TranscriptionTimestamps => pb::FieldId::TranscriptionTimestamps,
    }
}

fn field_from_proto(field: i32) -> Result<FieldId> {
    match pb::FieldId::try_from(field) {
        Ok(pb::FieldId::DecisionInputJson) => Ok(FieldId::DecisionInputJSON),
        Ok(pb::FieldId::DecisionInputPartImage) => Ok(FieldId::DecisionInputPartImage),
        Ok(pb::FieldId::DecisionInputPartText) => Ok(FieldId::DecisionInputPartText),
        Ok(pb::FieldId::DecisionInputText) => Ok(FieldId::DecisionInputText),
        Ok(pb::FieldId::DecisionQuestionChoices) => Ok(FieldId::DecisionQuestionChoices),
        Ok(pb::FieldId::DecisionQuestionInstructions) => Ok(FieldId::DecisionQuestionInstructions),
        Ok(pb::FieldId::DecisionQuestionKind) => Ok(FieldId::DecisionQuestionKind),
        Ok(pb::FieldId::DecisionQuestionLevels) => Ok(FieldId::DecisionQuestionLevels),
        Ok(pb::FieldId::DecisionQuestionName) => Ok(FieldId::DecisionQuestionName),
        Ok(pb::FieldId::DecisionRequestMetadata) => Ok(FieldId::DecisionRequestMetadata),
        Ok(pb::FieldId::EmbedDimensions) => Ok(FieldId::EmbedDimensions),
        Ok(pb::FieldId::EmbedItems) => Ok(FieldId::EmbedItems),
        Ok(pb::FieldId::EmbedItemAudio) => Ok(FieldId::EmbedItemAudio),
        Ok(pb::FieldId::EmbedItemData) => Ok(FieldId::EmbedItemData),
        Ok(pb::FieldId::EmbedItemFile) => Ok(FieldId::EmbedItemFile),
        Ok(pb::FieldId::EmbedItemImage) => Ok(FieldId::EmbedItemImage),
        Ok(pb::FieldId::EmbedItemMultiPart) => Ok(FieldId::EmbedItemMultiPart),
        Ok(pb::FieldId::EmbedItemText) => Ok(FieldId::EmbedItemText),
        Ok(pb::FieldId::EmbedItemToolCall) => Ok(FieldId::EmbedItemToolCall),
        Ok(pb::FieldId::EmbedItemToolResult) => Ok(FieldId::EmbedItemToolResult),
        Ok(pb::FieldId::EmbedItemVideo) => Ok(FieldId::EmbedItemVideo),
        Ok(pb::FieldId::GenerateContextAudio) => Ok(FieldId::GenerateContextAudio),
        Ok(pb::FieldId::GenerateContextData) => Ok(FieldId::GenerateContextData),
        Ok(pb::FieldId::GenerateContextFile) => Ok(FieldId::GenerateContextFile),
        Ok(pb::FieldId::GenerateContextImage) => Ok(FieldId::GenerateContextImage),
        Ok(pb::FieldId::GenerateContextReasoning) => Ok(FieldId::GenerateContextReasoning),
        Ok(pb::FieldId::GenerateContextText) => Ok(FieldId::GenerateContextText),
        Ok(pb::FieldId::GenerateContextToolCall) => Ok(FieldId::GenerateContextToolCall),
        Ok(pb::FieldId::GenerateContextToolResult) => Ok(FieldId::GenerateContextToolResult),
        Ok(pb::FieldId::GenerateContextVideo) => Ok(FieldId::GenerateContextVideo),
        Ok(pb::FieldId::GenerateContextRole) => Ok(FieldId::GenerateContextRole),
        Ok(pb::FieldId::GenerateExecutionStream) => Ok(FieldId::GenerateExecutionStream),
        Ok(pb::FieldId::GenerateExecutionUnary) => Ok(FieldId::GenerateExecutionUnary),
        Ok(pb::FieldId::GenerateIntentAudio) => Ok(FieldId::GenerateIntentAudio),
        Ok(pb::FieldId::GenerateIntentAudioCount) => Ok(FieldId::GenerateIntentAudioCount),
        Ok(pb::FieldId::GenerateIntentAudioFormat) => Ok(FieldId::GenerateIntentAudioFormat),
        Ok(pb::FieldId::GenerateIntentAudioFormatChannels) => {
            Ok(FieldId::GenerateIntentAudioFormatChannels)
        }
        Ok(pb::FieldId::GenerateIntentAudioFormatEncoding) => {
            Ok(FieldId::GenerateIntentAudioFormatEncoding)
        }
        Ok(pb::FieldId::GenerateIntentAudioFormatSampleRate) => {
            Ok(FieldId::GenerateIntentAudioFormatSampleRate)
        }
        Ok(pb::FieldId::GenerateIntentAudioSpeed) => Ok(FieldId::GenerateIntentAudioSpeed),
        Ok(pb::FieldId::GenerateIntentAudioVoice) => Ok(FieldId::GenerateIntentAudioVoice),
        Ok(pb::FieldId::GenerateIntentAudioVoiceId) => Ok(FieldId::GenerateIntentAudioVoiceID),
        Ok(pb::FieldId::GenerateIntentAudioVoiceLanguage) => {
            Ok(FieldId::GenerateIntentAudioVoiceLanguage)
        }
        Ok(pb::FieldId::GenerateIntentImage) => Ok(FieldId::GenerateIntentImage),
        Ok(pb::FieldId::GenerateIntentImageCount) => Ok(FieldId::GenerateIntentImageCount),
        Ok(pb::FieldId::GenerateIntentImageDelivery) => Ok(FieldId::GenerateIntentImageDelivery),
        Ok(pb::FieldId::GenerateIntentImageOutputFormat) => {
            Ok(FieldId::GenerateIntentImageOutputFormat)
        }
        Ok(pb::FieldId::GenerateIntentImageQuality) => Ok(FieldId::GenerateIntentImageQuality),
        Ok(pb::FieldId::GenerateIntentImageSeed) => Ok(FieldId::GenerateIntentImageSeed),
        Ok(pb::FieldId::GenerateIntentImageSize) => Ok(FieldId::GenerateIntentImageSize),
        Ok(pb::FieldId::GenerateIntentImageSizeHeight) => {
            Ok(FieldId::GenerateIntentImageSizeHeight)
        }
        Ok(pb::FieldId::GenerateIntentImageSizeWidth) => Ok(FieldId::GenerateIntentImageSizeWidth),
        Ok(pb::FieldId::GenerateIntentText) => Ok(FieldId::GenerateIntentText),
        Ok(pb::FieldId::GenerateIntentTextMaxOutputTokens) => {
            Ok(FieldId::GenerateIntentTextMaxOutputTokens)
        }
        Ok(pb::FieldId::GenerateIntentReasoning) => Ok(FieldId::GenerateIntentReasoning),
        Ok(pb::FieldId::GenerateIntentReasoningEffort) => {
            Ok(FieldId::GenerateIntentReasoningEffort)
        }
        Ok(pb::FieldId::GenerateIntentReasoningEnabled) => {
            Ok(FieldId::GenerateIntentReasoningEnabled)
        }
        Ok(pb::FieldId::GenerateIntentTextResponse) => Ok(FieldId::GenerateIntentTextResponse),
        Ok(pb::FieldId::GenerateIntentTextResponseKind) => {
            Ok(FieldId::GenerateIntentTextResponseKind)
        }
        Ok(pb::FieldId::GenerateIntentTextResponseName) => {
            Ok(FieldId::GenerateIntentTextResponseName)
        }
        Ok(pb::FieldId::GenerateIntentTextResponseSchema) => {
            Ok(FieldId::GenerateIntentTextResponseSchema)
        }
        Ok(pb::FieldId::GenerateIntentTemperature) => Ok(FieldId::GenerateIntentTemperature),
        Ok(pb::FieldId::GenerateIntentToolChoice) => Ok(FieldId::GenerateIntentToolChoice),
        Ok(pb::FieldId::GenerateIntentToolChoiceKind) => Ok(FieldId::GenerateIntentToolChoiceKind),
        Ok(pb::FieldId::GenerateIntentToolChoiceName) => Ok(FieldId::GenerateIntentToolChoiceName),
        Ok(pb::FieldId::GenerateIntentTools) => Ok(FieldId::GenerateIntentTools),
        Ok(pb::FieldId::GenerateIntentTopP) => Ok(FieldId::GenerateIntentTopP),
        Ok(pb::FieldId::GenerateIntentVideo) => Ok(FieldId::GenerateIntentVideo),
        Ok(pb::FieldId::GenerateIntentVideoAspectRatio) => {
            Ok(FieldId::GenerateIntentVideoAspectRatio)
        }
        Ok(pb::FieldId::GenerateIntentVideoDuration) => Ok(FieldId::GenerateIntentVideoDuration),
        Ok(pb::FieldId::GenerateIntentVideoResolution) => {
            Ok(FieldId::GenerateIntentVideoResolution)
        }
        Ok(pb::FieldId::GenerateIntentVideoSeed) => Ok(FieldId::GenerateIntentVideoSeed),
        Ok(pb::FieldId::GenerateIntentVideoWatermark) => Ok(FieldId::GenerateIntentVideoWatermark),
        Ok(pb::FieldId::GenerateInputAudio) => Ok(FieldId::GenerateInputAudio),
        Ok(pb::FieldId::GenerateInputData) => Ok(FieldId::GenerateInputData),
        Ok(pb::FieldId::GenerateInputFile) => Ok(FieldId::GenerateInputFile),
        Ok(pb::FieldId::GenerateInputImage) => Ok(FieldId::GenerateInputImage),
        Ok(pb::FieldId::GenerateInputReasoning) => Ok(FieldId::GenerateInputReasoning),
        Ok(pb::FieldId::GenerateInputText) => Ok(FieldId::GenerateInputText),
        Ok(pb::FieldId::GenerateInputToolCall) => Ok(FieldId::GenerateInputToolCall),
        Ok(pb::FieldId::GenerateInputToolResult) => Ok(FieldId::GenerateInputToolResult),
        Ok(pb::FieldId::GenerateInputVideo) => Ok(FieldId::GenerateInputVideo),
        Ok(pb::FieldId::GenerateInputRole) => Ok(FieldId::GenerateInputRole),
        Ok(pb::FieldId::GenerateRequestMetadata) => Ok(FieldId::GenerateRequestMetadata),
        Ok(pb::FieldId::TranscriptionAudio) => Ok(FieldId::TranscriptionAudio),
        Ok(pb::FieldId::TranscriptionLanguage) => Ok(FieldId::TranscriptionLanguage),
        Ok(pb::FieldId::TranscriptionPrompt) => Ok(FieldId::TranscriptionPrompt),
        Ok(pb::FieldId::TranscriptionInputFormat) => Ok(FieldId::TranscriptionInputFormat),
        Ok(pb::FieldId::TranscriptionTimestamps) => Ok(FieldId::TranscriptionTimestamps),
        _ => Err(Error::validation(format!(
            "inference: field enum {field} is not a canonical field"
        ))),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::inference::model::ComponentNote;
    use crate::inference::request::ToolChoice;

    fn request() -> GenerateRequest {
        GenerateRequest {
            model: "gpt-5".to_owned(),
            messages: vec![msg::Message {
                role: msg::Role::User as i32,
                content: Some(msg::Content {
                    parts: vec![msg::Part {
                        kind: Some(msg::part::Kind::Text(msg::TextPart {
                            text: "hi".to_owned(),
                        })),
                    }],
                }),
            }],
            intent: TextIntent {
                max_output_tokens: Some(32),
                temperature: Some(0.2),
                tools: vec![msg::ToolDefinition {
                    name: "clock".to_owned(),
                    description: "what time is it".to_owned(),
                    input_schema: br#"{"type":"object"}"#.to_vec(),
                }],
                tool_choice: ToolChoice::Required,
                format: TextFormat::JsonSchema {
                    name: "answer".to_owned(),
                    schema: r#"{"type":"object"}"#.to_owned(),
                },
                reasoning_effort: Some("high".to_owned()),
            },
        }
    }

    fn report() -> CompileReport {
        CompileReport::new(
            Operation::Generate,
            vec![
                Decision {
                    field: FieldId::GenerateExecutionUnary,
                    disposition: Disposition::Native,
                    reason: String::new(),
                    components: Vec::new(),
                },
                Decision {
                    field: FieldId::GenerateContextImage,
                    disposition: Disposition::Dropped,
                    reason: "no image channel".to_owned(),
                    components: vec![ComponentNote {
                        kind: "image".to_owned(),
                        disposition: Disposition::Dropped,
                        index: 2,
                        reason: "no channel".to_owned(),
                    }],
                },
            ],
        )
    }

    /// The two representations, pinned against each other: every field has a wire value,
    /// the values are distinct, and the mapping is a round-trip.
    #[test]
    fn every_field_has_exactly_one_wire_value() {
        let mut seen = std::collections::BTreeSet::new();
        for field in FieldId::ALL {
            let wire = field_to_proto(*field);
            assert_ne!(
                wire,
                pb::FieldId::Unspecified,
                "{field} maps to unspecified"
            );
            assert!(seen.insert(wire as i32), "two fields share {wire:?}");
            assert_eq!(
                field_from_proto(wire as i32).expect("decodes"),
                *field,
                "{field} does not survive the round trip"
            );
        }
        // And the unspecified value is not a field: a decision without one is an error
        // rather than a silently unlabelled entry.
        assert!(field_from_proto(pb::FieldId::Unspecified as i32).is_err());
    }

    #[test]
    fn a_request_round_trips() {
        let original = request();
        let back = request_from_proto(request_to_proto(&original)).expect("decodes");
        assert_eq!(back.model, original.model);
        assert_eq!(back.messages, original.messages);
        assert_eq!(back.intent, original.intent);
    }

    /// A tier is what a *client* sends; routing resolves it before an executor sees one.
    /// Seeing one here means the layer that owns it was skipped, which is an error rather
    /// than a model name to guess.
    #[test]
    fn a_tier_does_not_reach_the_executor() {
        let mut proto = request_to_proto(&request());
        proto.target = Some(pb::Target {
            choose: Some(pb::target::Choose::Tier("standard".to_owned())),
        });
        let error = request_from_proto(proto).expect_err("refused");
        assert_eq!(error.kind(), crate::error::ErrorKind::Validation);
        assert!(
            error.to_string().contains("routing resolves a tier"),
            "{error}"
        );
    }

    #[test]
    fn a_report_round_trips_with_its_component_notes() {
        let original = report();
        let back = report_from_proto(report_to_proto(&original)).expect("decodes");
        assert_eq!(back, original);
    }

    #[test]
    fn a_response_round_trips_with_its_evidence() {
        let response = GenerateResponse {
            parts: vec![msg::Part {
                kind: Some(msg::part::Kind::Text(msg::TextPart {
                    text: "hello".to_owned(),
                })),
            }],
            finish: FinishReason::Length,
            usage: Usage {
                input_tokens: 1,
                output_tokens: 2,
                total_tokens: 3,
                cache_read_tokens: None,
                reasoning_tokens: Some(4),
            },
        };
        let report = report();
        let (back, back_report) =
            response_from_proto(response_to_proto(&response, &report)).expect("decodes");
        assert_eq!(back, response);
        assert_eq!(back_report, report);
        assert_eq!(
            back.usage.cache_read_tokens, None,
            "an unreported counter must not come back as zero"
        );
    }

    #[test]
    fn events_round_trip_and_unknown_shapes_are_dropped() {
        let report = report();
        let delta = GenerateEvent::Delta("he".to_owned());
        let encoded = event_to_proto(&delta, Some(&report));
        assert_eq!(event_from_proto(encoded).expect("decodes"), Some(delta));

        let mut attempt = Attempt {
            target: "openai/gpt-5".to_owned(),
            phase: crate::inference::ledger::Phase::Execute,
            trigger: crate::inference::ledger::Trigger::Fallback,
            outcome: crate::inference::ledger::Outcome::Failed,
            error_kind: Some("not_available".to_owned()),
            observable_output: true,
            number: 2,
            backoff_millis: 150,
        };
        let encoded = event_to_proto(&GenerateEvent::Attempt(attempt.clone()), None);
        assert_eq!(
            event_from_proto(encoded).expect("decodes"),
            Some(GenerateEvent::Attempt(attempt.clone()))
        );
        // The vocabulary is spelled on the wire, so a peer can read it without this code.
        let encoded = event_to_proto(&GenerateEvent::Attempt(attempt.clone()), None);
        if let Some(pb::generate_event::Kind::Attempt(raw)) = encoded.kind {
            assert_eq!(raw.phase, "execute");
            assert_eq!(raw.trigger, "fallback");
            assert_eq!(raw.outcome, "failed");
        }
        attempt.number = 3;

        // A reasoning delta is not text, and a `Started` event is bookkeeping: neither is
        // inventing content.
        let reasoning = pb::GenerateEvent {
            kind: Some(pb::generate_event::Kind::Delta(msg::Delta {
                kind: Some(msg::delta::Kind::Reasoning("thinking".to_owned())),
            })),
        };
        assert_eq!(event_from_proto(reasoning).expect("decodes"), None);
    }
}
