//! The compile vocabulary: what a canonical request is made of.
//!
//! Ported from `core/inference/decision.go` and `core/inference/model/`. Nothing
//! here needs protobuf, tokio or tracing: these are the words a compiler, a driver
//! and an audit record agree on, and a caller that only needs to name a model should
//! not have to compile a wire stack to do it.
//!
//! Two properties are load-bearing, and both are pinned by tests:
//!
//! - **the strings are the contract.** A [`FieldId`] appears in checkpoint metadata,
//!   telemetry and compiled reports, so it is spelled exactly as the Go side spells
//!   it (`generate.context.*.content.parts.text`). The enum is the type-safe form; the
//!   string is what travels.
//! - **a successful compile covers every active field exactly once**, as `Native` or
//!   as `Dropped` with a reason. [`CompileReport::validate_success`] is that rule, and
//!   it is the only thing between an audit trail and a plausible-looking lie.

use std::fmt;

use crate::error::{Error, Result};

/// One canonical field of an inference request.
///
/// A `*` segment marks a field that aggregates whole content kinds across
/// occurrences (every message of a context, every item of an embedding batch); the
/// per-component distinction those lose lives in [`ComponentNote`].
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub enum FieldId {
    /// `decision.input.json`
    DecisionInputJSON,
    /// `decision.input.parts.image`
    DecisionInputPartImage,
    /// `decision.input.parts.text`
    DecisionInputPartText,
    /// `decision.input.text`
    DecisionInputText,
    /// `decision.questions.*.choices`
    DecisionQuestionChoices,
    /// `decision.questions.*.instructions`
    DecisionQuestionInstructions,
    /// `decision.questions.*.kind`
    DecisionQuestionKind,
    /// `decision.questions.*.levels`
    DecisionQuestionLevels,
    /// `decision.questions.*.name`
    DecisionQuestionName,
    /// `decision.request.metadata`
    DecisionRequestMetadata,
    /// `embed.dimensions`
    EmbedDimensions,
    /// `embed.items`
    EmbedItems,
    /// `embed.items.content.audio`
    EmbedItemAudio,
    /// `embed.items.content.data`
    EmbedItemData,
    /// `embed.items.content.file`
    EmbedItemFile,
    /// `embed.items.content.image`
    EmbedItemImage,
    /// `embed.items.content.multi_part`
    EmbedItemMultiPart,
    /// `embed.items.content.text`
    EmbedItemText,
    /// `embed.items.content.tool_call`
    EmbedItemToolCall,
    /// `embed.items.content.tool_result`
    EmbedItemToolResult,
    /// `embed.items.content.video`
    EmbedItemVideo,
    /// `generate.context.*.content.parts.audio`
    GenerateContextAudio,
    /// `generate.context.*.content.parts.data`
    GenerateContextData,
    /// `generate.context.*.content.parts.file`
    GenerateContextFile,
    /// `generate.context.*.content.parts.image`
    GenerateContextImage,
    /// `generate.context.*.content.parts.reasoning`
    GenerateContextReasoning,
    /// `generate.context.*.content.parts.text`
    GenerateContextText,
    /// `generate.context.*.content.parts.tool_call`
    GenerateContextToolCall,
    /// `generate.context.*.content.parts.tool_result`
    GenerateContextToolResult,
    /// `generate.context.*.content.parts.video`
    GenerateContextVideo,
    /// `generate.context.*.role`
    GenerateContextRole,
    /// `generate.execution.stream`
    GenerateExecutionStream,
    /// `generate.execution.unary`
    GenerateExecutionUnary,
    /// `generate.input.content.intent.audio`
    GenerateIntentAudio,
    /// `generate.input.content.intent.audio.count`
    GenerateIntentAudioCount,
    /// `generate.input.content.intent.audio.format`
    GenerateIntentAudioFormat,
    /// `generate.input.content.intent.audio.format.channels`
    GenerateIntentAudioFormatChannels,
    /// `generate.input.content.intent.audio.format.encoding`
    GenerateIntentAudioFormatEncoding,
    /// `generate.input.content.intent.audio.format.sample_rate_hz`
    GenerateIntentAudioFormatSampleRate,
    /// `generate.input.content.intent.audio.speed`
    GenerateIntentAudioSpeed,
    /// `generate.input.content.intent.audio.voice`
    GenerateIntentAudioVoice,
    /// `generate.input.content.intent.audio.voice.id`
    GenerateIntentAudioVoiceID,
    /// `generate.input.content.intent.audio.voice.language`
    GenerateIntentAudioVoiceLanguage,
    /// `generate.input.content.intent.image`
    GenerateIntentImage,
    /// `generate.input.content.intent.image.count`
    GenerateIntentImageCount,
    /// `generate.input.content.intent.image.delivery`
    GenerateIntentImageDelivery,
    /// `generate.input.content.intent.image.output_format`
    GenerateIntentImageOutputFormat,
    /// `generate.input.content.intent.image.quality`
    GenerateIntentImageQuality,
    /// `generate.input.content.intent.image.seed`
    GenerateIntentImageSeed,
    /// `generate.input.content.intent.image.size`
    GenerateIntentImageSize,
    /// `generate.input.content.intent.image.size.height`
    GenerateIntentImageSizeHeight,
    /// `generate.input.content.intent.image.size.width`
    GenerateIntentImageSizeWidth,
    /// `generate.input.content.intent.text`
    GenerateIntentText,
    /// `generate.input.content.intent.text.max_output_tokens`
    GenerateIntentTextMaxOutputTokens,
    /// `generate.input.content.intent.text.reasoning`
    GenerateIntentReasoning,
    /// `generate.input.content.intent.text.reasoning_effort`
    GenerateIntentReasoningEffort,
    /// `generate.input.content.intent.text.reasoning_enabled`
    GenerateIntentReasoningEnabled,
    /// `generate.input.content.intent.text.response`
    GenerateIntentTextResponse,
    /// `generate.input.content.intent.text.response.kind`
    GenerateIntentTextResponseKind,
    /// `generate.input.content.intent.text.response.name`
    GenerateIntentTextResponseName,
    /// `generate.input.content.intent.text.response.schema`
    GenerateIntentTextResponseSchema,
    /// `generate.input.content.intent.text.temperature`
    GenerateIntentTemperature,
    /// `generate.input.content.intent.text.tool_choice`
    GenerateIntentToolChoice,
    /// `generate.input.content.intent.text.tool_choice.kind`
    GenerateIntentToolChoiceKind,
    /// `generate.input.content.intent.text.tool_choice.name`
    GenerateIntentToolChoiceName,
    /// `generate.input.content.intent.text.tools`
    GenerateIntentTools,
    /// `generate.input.content.intent.text.top_p`
    GenerateIntentTopP,
    /// `generate.input.content.intent.video`
    GenerateIntentVideo,
    /// `generate.input.content.intent.video.aspect_ratio`
    GenerateIntentVideoAspectRatio,
    /// `generate.input.content.intent.video.duration_millis`
    GenerateIntentVideoDuration,
    /// `generate.input.content.intent.video.resolution`
    GenerateIntentVideoResolution,
    /// `generate.input.content.intent.video.seed`
    GenerateIntentVideoSeed,
    /// `generate.input.content.intent.video.watermark`
    GenerateIntentVideoWatermark,
    /// `generate.input.content.parts.audio`
    GenerateInputAudio,
    /// `generate.input.content.parts.data`
    GenerateInputData,
    /// `generate.input.content.parts.file`
    GenerateInputFile,
    /// `generate.input.content.parts.image`
    GenerateInputImage,
    /// `generate.input.content.parts.reasoning`
    GenerateInputReasoning,
    /// `generate.input.content.parts.text`
    GenerateInputText,
    /// `generate.input.content.parts.tool_call`
    GenerateInputToolCall,
    /// `generate.input.content.parts.tool_result`
    GenerateInputToolResult,
    /// `generate.input.content.parts.video`
    GenerateInputVideo,
    /// `generate.input.role`
    GenerateInputRole,
    /// `generate.request.metadata`
    GenerateRequestMetadata,
    /// `transcription.audio`
    TranscriptionAudio,
    /// `transcription.language`
    TranscriptionLanguage,
    /// `transcription.prompt`
    TranscriptionPrompt,
    /// `transcription.session.input_format`
    TranscriptionInputFormat,
    /// `transcription.timestamps`
    TranscriptionTimestamps,
}

impl FieldId {
    /// Every field, in string order.
    ///
    /// A table that has to cover the vocabulary (routing, validation) iterates this
    /// instead of listing variants again, so a new field cannot silently miss one.
    pub const ALL: &'static [Self] = &[
        Self::DecisionInputJSON,
        Self::DecisionInputPartImage,
        Self::DecisionInputPartText,
        Self::DecisionInputText,
        Self::DecisionQuestionChoices,
        Self::DecisionQuestionInstructions,
        Self::DecisionQuestionKind,
        Self::DecisionQuestionLevels,
        Self::DecisionQuestionName,
        Self::DecisionRequestMetadata,
        Self::EmbedDimensions,
        Self::EmbedItems,
        Self::EmbedItemAudio,
        Self::EmbedItemData,
        Self::EmbedItemFile,
        Self::EmbedItemImage,
        Self::EmbedItemMultiPart,
        Self::EmbedItemText,
        Self::EmbedItemToolCall,
        Self::EmbedItemToolResult,
        Self::EmbedItemVideo,
        Self::GenerateContextAudio,
        Self::GenerateContextData,
        Self::GenerateContextFile,
        Self::GenerateContextImage,
        Self::GenerateContextReasoning,
        Self::GenerateContextText,
        Self::GenerateContextToolCall,
        Self::GenerateContextToolResult,
        Self::GenerateContextVideo,
        Self::GenerateContextRole,
        Self::GenerateExecutionStream,
        Self::GenerateExecutionUnary,
        Self::GenerateIntentAudio,
        Self::GenerateIntentAudioCount,
        Self::GenerateIntentAudioFormat,
        Self::GenerateIntentAudioFormatChannels,
        Self::GenerateIntentAudioFormatEncoding,
        Self::GenerateIntentAudioFormatSampleRate,
        Self::GenerateIntentAudioSpeed,
        Self::GenerateIntentAudioVoice,
        Self::GenerateIntentAudioVoiceID,
        Self::GenerateIntentAudioVoiceLanguage,
        Self::GenerateIntentImage,
        Self::GenerateIntentImageCount,
        Self::GenerateIntentImageDelivery,
        Self::GenerateIntentImageOutputFormat,
        Self::GenerateIntentImageQuality,
        Self::GenerateIntentImageSeed,
        Self::GenerateIntentImageSize,
        Self::GenerateIntentImageSizeHeight,
        Self::GenerateIntentImageSizeWidth,
        Self::GenerateIntentText,
        Self::GenerateIntentTextMaxOutputTokens,
        Self::GenerateIntentReasoning,
        Self::GenerateIntentReasoningEffort,
        Self::GenerateIntentReasoningEnabled,
        Self::GenerateIntentTextResponse,
        Self::GenerateIntentTextResponseKind,
        Self::GenerateIntentTextResponseName,
        Self::GenerateIntentTextResponseSchema,
        Self::GenerateIntentTemperature,
        Self::GenerateIntentToolChoice,
        Self::GenerateIntentToolChoiceKind,
        Self::GenerateIntentToolChoiceName,
        Self::GenerateIntentTools,
        Self::GenerateIntentTopP,
        Self::GenerateIntentVideo,
        Self::GenerateIntentVideoAspectRatio,
        Self::GenerateIntentVideoDuration,
        Self::GenerateIntentVideoResolution,
        Self::GenerateIntentVideoSeed,
        Self::GenerateIntentVideoWatermark,
        Self::GenerateInputAudio,
        Self::GenerateInputData,
        Self::GenerateInputFile,
        Self::GenerateInputImage,
        Self::GenerateInputReasoning,
        Self::GenerateInputText,
        Self::GenerateInputToolCall,
        Self::GenerateInputToolResult,
        Self::GenerateInputVideo,
        Self::GenerateInputRole,
        Self::GenerateRequestMetadata,
        Self::TranscriptionAudio,
        Self::TranscriptionLanguage,
        Self::TranscriptionPrompt,
        Self::TranscriptionInputFormat,
        Self::TranscriptionTimestamps,
    ];

    /// The wire and JSON spelling.
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::DecisionInputJSON => "decision.input.json",
            Self::DecisionInputPartImage => "decision.input.parts.image",
            Self::DecisionInputPartText => "decision.input.parts.text",
            Self::DecisionInputText => "decision.input.text",
            Self::DecisionQuestionChoices => "decision.questions.*.choices",
            Self::DecisionQuestionInstructions => "decision.questions.*.instructions",
            Self::DecisionQuestionKind => "decision.questions.*.kind",
            Self::DecisionQuestionLevels => "decision.questions.*.levels",
            Self::DecisionQuestionName => "decision.questions.*.name",
            Self::DecisionRequestMetadata => "decision.request.metadata",
            Self::EmbedDimensions => "embed.dimensions",
            Self::EmbedItems => "embed.items",
            Self::EmbedItemAudio => "embed.items.content.audio",
            Self::EmbedItemData => "embed.items.content.data",
            Self::EmbedItemFile => "embed.items.content.file",
            Self::EmbedItemImage => "embed.items.content.image",
            Self::EmbedItemMultiPart => "embed.items.content.multi_part",
            Self::EmbedItemText => "embed.items.content.text",
            Self::EmbedItemToolCall => "embed.items.content.tool_call",
            Self::EmbedItemToolResult => "embed.items.content.tool_result",
            Self::EmbedItemVideo => "embed.items.content.video",
            Self::GenerateContextAudio => "generate.context.*.content.parts.audio",
            Self::GenerateContextData => "generate.context.*.content.parts.data",
            Self::GenerateContextFile => "generate.context.*.content.parts.file",
            Self::GenerateContextImage => "generate.context.*.content.parts.image",
            Self::GenerateContextReasoning => "generate.context.*.content.parts.reasoning",
            Self::GenerateContextText => "generate.context.*.content.parts.text",
            Self::GenerateContextToolCall => "generate.context.*.content.parts.tool_call",
            Self::GenerateContextToolResult => "generate.context.*.content.parts.tool_result",
            Self::GenerateContextVideo => "generate.context.*.content.parts.video",
            Self::GenerateContextRole => "generate.context.*.role",
            Self::GenerateExecutionStream => "generate.execution.stream",
            Self::GenerateExecutionUnary => "generate.execution.unary",
            Self::GenerateIntentAudio => "generate.input.content.intent.audio",
            Self::GenerateIntentAudioCount => "generate.input.content.intent.audio.count",
            Self::GenerateIntentAudioFormat => "generate.input.content.intent.audio.format",
            Self::GenerateIntentAudioFormatChannels => {
                "generate.input.content.intent.audio.format.channels"
            }
            Self::GenerateIntentAudioFormatEncoding => {
                "generate.input.content.intent.audio.format.encoding"
            }
            Self::GenerateIntentAudioFormatSampleRate => {
                "generate.input.content.intent.audio.format.sample_rate_hz"
            }
            Self::GenerateIntentAudioSpeed => "generate.input.content.intent.audio.speed",
            Self::GenerateIntentAudioVoice => "generate.input.content.intent.audio.voice",
            Self::GenerateIntentAudioVoiceID => "generate.input.content.intent.audio.voice.id",
            Self::GenerateIntentAudioVoiceLanguage => {
                "generate.input.content.intent.audio.voice.language"
            }
            Self::GenerateIntentImage => "generate.input.content.intent.image",
            Self::GenerateIntentImageCount => "generate.input.content.intent.image.count",
            Self::GenerateIntentImageDelivery => "generate.input.content.intent.image.delivery",
            Self::GenerateIntentImageOutputFormat => {
                "generate.input.content.intent.image.output_format"
            }
            Self::GenerateIntentImageQuality => "generate.input.content.intent.image.quality",
            Self::GenerateIntentImageSeed => "generate.input.content.intent.image.seed",
            Self::GenerateIntentImageSize => "generate.input.content.intent.image.size",
            Self::GenerateIntentImageSizeHeight => {
                "generate.input.content.intent.image.size.height"
            }
            Self::GenerateIntentImageSizeWidth => "generate.input.content.intent.image.size.width",
            Self::GenerateIntentText => "generate.input.content.intent.text",
            Self::GenerateIntentTextMaxOutputTokens => {
                "generate.input.content.intent.text.max_output_tokens"
            }
            Self::GenerateIntentReasoning => "generate.input.content.intent.text.reasoning",
            Self::GenerateIntentReasoningEffort => {
                "generate.input.content.intent.text.reasoning_effort"
            }
            Self::GenerateIntentReasoningEnabled => {
                "generate.input.content.intent.text.reasoning_enabled"
            }
            Self::GenerateIntentTextResponse => "generate.input.content.intent.text.response",
            Self::GenerateIntentTextResponseKind => {
                "generate.input.content.intent.text.response.kind"
            }
            Self::GenerateIntentTextResponseName => {
                "generate.input.content.intent.text.response.name"
            }
            Self::GenerateIntentTextResponseSchema => {
                "generate.input.content.intent.text.response.schema"
            }
            Self::GenerateIntentTemperature => "generate.input.content.intent.text.temperature",
            Self::GenerateIntentToolChoice => "generate.input.content.intent.text.tool_choice",
            Self::GenerateIntentToolChoiceKind => {
                "generate.input.content.intent.text.tool_choice.kind"
            }
            Self::GenerateIntentToolChoiceName => {
                "generate.input.content.intent.text.tool_choice.name"
            }
            Self::GenerateIntentTools => "generate.input.content.intent.text.tools",
            Self::GenerateIntentTopP => "generate.input.content.intent.text.top_p",
            Self::GenerateIntentVideo => "generate.input.content.intent.video",
            Self::GenerateIntentVideoAspectRatio => {
                "generate.input.content.intent.video.aspect_ratio"
            }
            Self::GenerateIntentVideoDuration => {
                "generate.input.content.intent.video.duration_millis"
            }
            Self::GenerateIntentVideoResolution => "generate.input.content.intent.video.resolution",
            Self::GenerateIntentVideoSeed => "generate.input.content.intent.video.seed",
            Self::GenerateIntentVideoWatermark => "generate.input.content.intent.video.watermark",
            Self::GenerateInputAudio => "generate.input.content.parts.audio",
            Self::GenerateInputData => "generate.input.content.parts.data",
            Self::GenerateInputFile => "generate.input.content.parts.file",
            Self::GenerateInputImage => "generate.input.content.parts.image",
            Self::GenerateInputReasoning => "generate.input.content.parts.reasoning",
            Self::GenerateInputText => "generate.input.content.parts.text",
            Self::GenerateInputToolCall => "generate.input.content.parts.tool_call",
            Self::GenerateInputToolResult => "generate.input.content.parts.tool_result",
            Self::GenerateInputVideo => "generate.input.content.parts.video",
            Self::GenerateInputRole => "generate.input.role",
            Self::GenerateRequestMetadata => "generate.request.metadata",
            Self::TranscriptionAudio => "transcription.audio",
            Self::TranscriptionLanguage => "transcription.language",
            Self::TranscriptionPrompt => "transcription.prompt",
            Self::TranscriptionInputFormat => "transcription.session.input_format",
            Self::TranscriptionTimestamps => "transcription.timestamps",
        }
    }

    /// Decode a spelling from a peer, a checkpoint or a document.
    pub fn parse(raw: &str) -> Result<Self> {
        match raw {
            "decision.input.json" => Ok(Self::DecisionInputJSON),
            "decision.input.parts.image" => Ok(Self::DecisionInputPartImage),
            "decision.input.parts.text" => Ok(Self::DecisionInputPartText),
            "decision.input.text" => Ok(Self::DecisionInputText),
            "decision.questions.*.choices" => Ok(Self::DecisionQuestionChoices),
            "decision.questions.*.instructions" => Ok(Self::DecisionQuestionInstructions),
            "decision.questions.*.kind" => Ok(Self::DecisionQuestionKind),
            "decision.questions.*.levels" => Ok(Self::DecisionQuestionLevels),
            "decision.questions.*.name" => Ok(Self::DecisionQuestionName),
            "decision.request.metadata" => Ok(Self::DecisionRequestMetadata),
            "embed.dimensions" => Ok(Self::EmbedDimensions),
            "embed.items" => Ok(Self::EmbedItems),
            "embed.items.content.audio" => Ok(Self::EmbedItemAudio),
            "embed.items.content.data" => Ok(Self::EmbedItemData),
            "embed.items.content.file" => Ok(Self::EmbedItemFile),
            "embed.items.content.image" => Ok(Self::EmbedItemImage),
            "embed.items.content.multi_part" => Ok(Self::EmbedItemMultiPart),
            "embed.items.content.text" => Ok(Self::EmbedItemText),
            "embed.items.content.tool_call" => Ok(Self::EmbedItemToolCall),
            "embed.items.content.tool_result" => Ok(Self::EmbedItemToolResult),
            "embed.items.content.video" => Ok(Self::EmbedItemVideo),
            "generate.context.*.content.parts.audio" => Ok(Self::GenerateContextAudio),
            "generate.context.*.content.parts.data" => Ok(Self::GenerateContextData),
            "generate.context.*.content.parts.file" => Ok(Self::GenerateContextFile),
            "generate.context.*.content.parts.image" => Ok(Self::GenerateContextImage),
            "generate.context.*.content.parts.reasoning" => Ok(Self::GenerateContextReasoning),
            "generate.context.*.content.parts.text" => Ok(Self::GenerateContextText),
            "generate.context.*.content.parts.tool_call" => Ok(Self::GenerateContextToolCall),
            "generate.context.*.content.parts.tool_result" => Ok(Self::GenerateContextToolResult),
            "generate.context.*.content.parts.video" => Ok(Self::GenerateContextVideo),
            "generate.context.*.role" => Ok(Self::GenerateContextRole),
            "generate.execution.stream" => Ok(Self::GenerateExecutionStream),
            "generate.execution.unary" => Ok(Self::GenerateExecutionUnary),
            "generate.input.content.intent.audio" => Ok(Self::GenerateIntentAudio),
            "generate.input.content.intent.audio.count" => Ok(Self::GenerateIntentAudioCount),
            "generate.input.content.intent.audio.format" => Ok(Self::GenerateIntentAudioFormat),
            "generate.input.content.intent.audio.format.channels" => {
                Ok(Self::GenerateIntentAudioFormatChannels)
            }
            "generate.input.content.intent.audio.format.encoding" => {
                Ok(Self::GenerateIntentAudioFormatEncoding)
            }
            "generate.input.content.intent.audio.format.sample_rate_hz" => {
                Ok(Self::GenerateIntentAudioFormatSampleRate)
            }
            "generate.input.content.intent.audio.speed" => Ok(Self::GenerateIntentAudioSpeed),
            "generate.input.content.intent.audio.voice" => Ok(Self::GenerateIntentAudioVoice),
            "generate.input.content.intent.audio.voice.id" => Ok(Self::GenerateIntentAudioVoiceID),
            "generate.input.content.intent.audio.voice.language" => {
                Ok(Self::GenerateIntentAudioVoiceLanguage)
            }
            "generate.input.content.intent.image" => Ok(Self::GenerateIntentImage),
            "generate.input.content.intent.image.count" => Ok(Self::GenerateIntentImageCount),
            "generate.input.content.intent.image.delivery" => Ok(Self::GenerateIntentImageDelivery),
            "generate.input.content.intent.image.output_format" => {
                Ok(Self::GenerateIntentImageOutputFormat)
            }
            "generate.input.content.intent.image.quality" => Ok(Self::GenerateIntentImageQuality),
            "generate.input.content.intent.image.seed" => Ok(Self::GenerateIntentImageSeed),
            "generate.input.content.intent.image.size" => Ok(Self::GenerateIntentImageSize),
            "generate.input.content.intent.image.size.height" => {
                Ok(Self::GenerateIntentImageSizeHeight)
            }
            "generate.input.content.intent.image.size.width" => {
                Ok(Self::GenerateIntentImageSizeWidth)
            }
            "generate.input.content.intent.text" => Ok(Self::GenerateIntentText),
            "generate.input.content.intent.text.max_output_tokens" => {
                Ok(Self::GenerateIntentTextMaxOutputTokens)
            }
            "generate.input.content.intent.text.reasoning" => Ok(Self::GenerateIntentReasoning),
            "generate.input.content.intent.text.reasoning_effort" => {
                Ok(Self::GenerateIntentReasoningEffort)
            }
            "generate.input.content.intent.text.reasoning_enabled" => {
                Ok(Self::GenerateIntentReasoningEnabled)
            }
            "generate.input.content.intent.text.response" => Ok(Self::GenerateIntentTextResponse),
            "generate.input.content.intent.text.response.kind" => {
                Ok(Self::GenerateIntentTextResponseKind)
            }
            "generate.input.content.intent.text.response.name" => {
                Ok(Self::GenerateIntentTextResponseName)
            }
            "generate.input.content.intent.text.response.schema" => {
                Ok(Self::GenerateIntentTextResponseSchema)
            }
            "generate.input.content.intent.text.temperature" => Ok(Self::GenerateIntentTemperature),
            "generate.input.content.intent.text.tool_choice" => Ok(Self::GenerateIntentToolChoice),
            "generate.input.content.intent.text.tool_choice.kind" => {
                Ok(Self::GenerateIntentToolChoiceKind)
            }
            "generate.input.content.intent.text.tool_choice.name" => {
                Ok(Self::GenerateIntentToolChoiceName)
            }
            "generate.input.content.intent.text.tools" => Ok(Self::GenerateIntentTools),
            "generate.input.content.intent.text.top_p" => Ok(Self::GenerateIntentTopP),
            "generate.input.content.intent.video" => Ok(Self::GenerateIntentVideo),
            "generate.input.content.intent.video.aspect_ratio" => {
                Ok(Self::GenerateIntentVideoAspectRatio)
            }
            "generate.input.content.intent.video.duration_millis" => {
                Ok(Self::GenerateIntentVideoDuration)
            }
            "generate.input.content.intent.video.resolution" => {
                Ok(Self::GenerateIntentVideoResolution)
            }
            "generate.input.content.intent.video.seed" => Ok(Self::GenerateIntentVideoSeed),
            "generate.input.content.intent.video.watermark" => {
                Ok(Self::GenerateIntentVideoWatermark)
            }
            "generate.input.content.parts.audio" => Ok(Self::GenerateInputAudio),
            "generate.input.content.parts.data" => Ok(Self::GenerateInputData),
            "generate.input.content.parts.file" => Ok(Self::GenerateInputFile),
            "generate.input.content.parts.image" => Ok(Self::GenerateInputImage),
            "generate.input.content.parts.reasoning" => Ok(Self::GenerateInputReasoning),
            "generate.input.content.parts.text" => Ok(Self::GenerateInputText),
            "generate.input.content.parts.tool_call" => Ok(Self::GenerateInputToolCall),
            "generate.input.content.parts.tool_result" => Ok(Self::GenerateInputToolResult),
            "generate.input.content.parts.video" => Ok(Self::GenerateInputVideo),
            "generate.input.role" => Ok(Self::GenerateInputRole),
            "generate.request.metadata" => Ok(Self::GenerateRequestMetadata),
            "transcription.audio" => Ok(Self::TranscriptionAudio),
            "transcription.language" => Ok(Self::TranscriptionLanguage),
            "transcription.prompt" => Ok(Self::TranscriptionPrompt),
            "transcription.session.input_format" => Ok(Self::TranscriptionInputFormat),
            "transcription.timestamps" => Ok(Self::TranscriptionTimestamps),
            other => Err(Error::validation(format!(
                "inference: {other:?} is not a canonical field"
            ))),
        }
    }
}

impl fmt::Display for FieldId {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

impl serde::Serialize for FieldId {
    fn serialize<S: serde::Serializer>(
        &self,
        serializer: S,
    ) -> std::result::Result<S::Ok, S::Error> {
        serializer.serialize_str(self.as_str())
    }
}

impl<'de> serde::Deserialize<'de> for FieldId {
    fn deserialize<D: serde::Deserializer<'de>>(
        deserializer: D,
    ) -> std::result::Result<Self, D::Error> {
        let raw = String::deserialize(deserializer)?;
        Self::parse(&raw).map_err(serde::de::Error::custom)
    }
}

/// What one workload is called: the operation axis of the vocabulary.
#[derive(
    Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash, serde::Serialize, serde::Deserialize,
)]
#[serde(rename_all = "snake_case")]
pub enum Operation {
    /// Text, image, audio and video generation.
    Generate,
    /// Embedding.
    Embed,
    /// Speech to text.
    Transcription,
    /// A constrained classification call.
    Decision,
}

impl Operation {
    /// Every workload, in declaration order.
    pub const ALL: &'static [Self] = &[
        Self::Generate,
        Self::Embed,
        Self::Transcription,
        Self::Decision,
    ];

    /// The wire and JSON spelling.
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Generate => "generate",
            Self::Embed => "embed",
            Self::Transcription => "transcription",
            Self::Decision => "decision",
        }
    }

    /// Decode a spelling.
    pub fn parse(raw: &str) -> Result<Self> {
        match raw {
            "generate" => Ok(Self::Generate),
            "embed" => Ok(Self::Embed),
            "transcription" => Ok(Self::Transcription),
            "decision" => Ok(Self::Decision),
            other => Err(Error::validation(format!(
                "inference: {other:?} is not an operation"
            ))),
        }
    }
}

impl fmt::Display for Operation {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// How one canonical field fared in a compile.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Disposition {
    /// The compiler consumed the field into the provider's wire.
    Native,
    /// The field cannot be honoured, and the compile failed.
    Rejected,
    /// The compiler intentionally discarded the field: it cannot round-trip or has no
    /// channel, and the reason says why. A drop is an audit, not a silent loss.
    Dropped,
}

impl Disposition {
    /// The wire and JSON spelling.
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Native => "native",
            Self::Rejected => "rejected",
            Self::Dropped => "dropped",
        }
    }

    /// Whether this disposition ends a field's story for a successful compile.
    #[must_use]
    pub const fn is_terminal_success(self) -> bool {
        matches!(self, Self::Native | Self::Dropped)
    }
}

impl fmt::Display for Disposition {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// How one component inside a canonical field fared.
///
/// A field-level disposition cannot say "the text arrived but the image did not"; the
/// notes keep that machine-readable without widening the field table or letting one
/// field carry several terminal dispositions.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub struct ComponentNote {
    /// The canonical content kind this note covers (`text`, `image`, …).
    pub kind: String,
    /// This component's own terminal disposition.
    pub disposition: Disposition,
    /// The component's position in encounter order. Drivers choose the granularity
    /// the field aggregates at, and indexes are unique within one decision.
    pub index: usize,
    /// Why a non-native component was degraded.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub reason: String,
}

/// How one field fared, with per-component detail when it lost a part.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub struct Decision {
    /// The field.
    pub field: FieldId,
    /// What happened to it.
    pub disposition: Disposition,
    /// Why, for anything that is not `native`.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub reason: String,
    /// Per-component detail. Populated only when the field lost at least one
    /// component; it then covers every component in encounter order, so `index` stays
    /// meaningful. The field's own disposition is the worst component's.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub components: Vec<ComponentNote>,
}

/// What one compile did, field by field: the evidence that replaces a handle.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub struct CompileReport {
    /// The workload this report is about.
    pub operation: Operation,
    /// One decision per field the compiler touched.
    pub decisions: Vec<Decision>,
}

impl CompileReport {
    /// Build a report.
    #[must_use]
    pub fn new(operation: Operation, decisions: Vec<Decision>) -> Self {
        Self {
            operation,
            decisions,
        }
    }

    /// Whether the field was rejected.
    #[must_use]
    pub fn rejects(&self, field: FieldId) -> bool {
        self.decisions.iter().any(|decision| {
            decision.field == field && decision.disposition == Disposition::Rejected
        })
    }

    /// Whether the field was dropped with a reason while the compile succeeded.
    #[must_use]
    pub fn dropped(&self, field: FieldId) -> bool {
        self.decisions
            .iter()
            .any(|decision| decision.field == field && decision.disposition == Disposition::Dropped)
    }

    /// The per-component notes for `field`, if any.
    #[must_use]
    pub fn components(&self, field: FieldId) -> Option<&[ComponentNote]> {
        self.decisions
            .iter()
            .find(|decision| decision.field == field)
            .map(|decision| decision.components.as_slice())
            .filter(|components| !components.is_empty())
    }

    /// Prove that a successful compile covered every active field exactly once.
    ///
    /// `active` is what the request actually carries. The rule is the audit trail's
    /// whole value: a field that is neither `native` nor `dropped`-with-a-reason was
    /// discarded and nobody wrote it down.
    pub fn validate_success(&self, operation: Operation, active: &[FieldId]) -> Result<()> {
        if self.operation != operation {
            return Err(Error::internal(format!(
                "inference: the compiler reported {}, not {}",
                self.operation, operation
            )));
        }
        let mut seen = std::collections::BTreeSet::new();
        for decision in &self.decisions {
            if !active.contains(&decision.field) {
                return Err(Error::internal(format!(
                    "inference: {} was reported for a field the request does not carry",
                    decision.field
                )));
            }
            if !seen.insert(decision.field) {
                return Err(Error::internal(format!(
                    "inference: {} was reported twice",
                    decision.field
                )));
            }
            match decision.disposition {
                Disposition::Native => {}
                Disposition::Dropped if !decision.reason.trim().is_empty() => {}
                Disposition::Dropped => {
                    return Err(Error::internal(format!(
                        "inference: {} was dropped without a reason",
                        decision.field
                    )));
                }
                Disposition::Rejected => {
                    return Err(Error::internal(format!(
                        "inference: {} was rejected, so the compile did not succeed",
                        decision.field
                    )));
                }
            }
        }
        for field in active {
            if !seen.contains(field) {
                return Err(Error::internal(format!(
                    "inference: {field} is active but was never reported"
                )));
            }
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The spellings are the contract: they travel in checkpoints, telemetry and
    /// reports, and a rename is invisible until something reads an old record.
    #[test]
    fn the_field_spellings_are_the_go_ones() {
        for (field, spelling) in [
            (FieldId::GenerateExecutionUnary, "generate.execution.unary"),
            (
                FieldId::GenerateContextText,
                "generate.context.*.content.parts.text",
            ),
            (
                FieldId::GenerateIntentToolChoiceName,
                "generate.input.content.intent.text.tool_choice.name",
            ),
            (FieldId::EmbedItems, "embed.items"),
            (FieldId::TranscriptionAudio, "transcription.audio"),
            (
                FieldId::DecisionQuestionLevels,
                "decision.questions.*.levels",
            ),
        ] {
            assert_eq!(field.as_str(), spelling);
            assert_eq!(FieldId::parse(spelling).expect("parses"), field);
        }
        assert!(FieldId::parse("generate.context.text").is_err());
    }

    /// Unique, and every variant is in `ALL`: the table that routing and validation
    /// iterate cannot miss one.
    #[test]
    fn the_table_covers_every_field_exactly_once() {
        let mut spellings: Vec<&str> = FieldId::ALL.iter().map(|field| field.as_str()).collect();
        spellings.sort_unstable();
        let count = spellings.len();
        spellings.dedup();
        assert_eq!(spellings.len(), count, "two fields share a spelling");
        assert_eq!(count, 89, "the vocabulary is pinned: {count} fields");
        for field in FieldId::ALL {
            assert_eq!(FieldId::parse(field.as_str()).expect("round trip"), *field);
        }
    }

    #[test]
    fn a_report_round_trips_through_json_in_the_go_shape() {
        let report = CompileReport::new(
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
                    reason: "this provider has no image channel".to_owned(),
                    components: vec![ComponentNote {
                        kind: "image".to_owned(),
                        disposition: Disposition::Dropped,
                        index: 0,
                        reason: "no channel".to_owned(),
                    }],
                },
            ],
        );
        let json = serde_json::to_value(&report).expect("serialize");
        assert_eq!(json["operation"], "generate");
        assert_eq!(json["decisions"][0]["field"], "generate.execution.unary");
        assert_eq!(json["decisions"][0]["disposition"], "native");
        // An empty reason is absent rather than empty, the way Go's `omitempty` reads.
        assert!(json["decisions"][0].get("reason").is_none());
        assert!(json["decisions"][0].get("components").is_none());
        assert_eq!(json["decisions"][1]["components"][0]["kind"], "image");
        let back: CompileReport = serde_json::from_value(json).expect("deserialize");
        assert_eq!(back, report);
    }

    #[test]
    fn a_successful_compile_must_report_every_active_field_once() {
        let active = [
            FieldId::GenerateExecutionUnary,
            FieldId::GenerateContextRole,
        ];
        let native = Decision {
            field: FieldId::GenerateExecutionUnary,
            disposition: Disposition::Native,
            reason: String::new(),
            components: Vec::new(),
        };
        let good = CompileReport::new(
            Operation::Generate,
            vec![
                native.clone(),
                Decision {
                    field: FieldId::GenerateContextRole,
                    disposition: Disposition::Dropped,
                    reason: "roles do not round-trip here".to_owned(),
                    components: Vec::new(),
                },
            ],
        );
        good.validate_success(Operation::Generate, &active)
            .expect("a complete report");

        // Missing a field.
        let incomplete = CompileReport::new(Operation::Generate, vec![native.clone()]);
        assert!(
            incomplete
                .validate_success(Operation::Generate, &active)
                .is_err()
        );

        // A field the request does not carry.
        let extra = CompileReport::new(
            Operation::Generate,
            vec![
                native.clone(),
                good.decisions[1].clone(),
                Decision {
                    field: FieldId::EmbedDimensions,
                    disposition: Disposition::Native,
                    reason: String::new(),
                    components: Vec::new(),
                },
            ],
        );
        assert!(
            extra
                .validate_success(Operation::Generate, &active)
                .is_err()
        );

        // Dropped without a reason is the failure this rule exists for.
        let silent = CompileReport::new(
            Operation::Generate,
            vec![
                native.clone(),
                Decision {
                    field: FieldId::GenerateContextRole,
                    disposition: Disposition::Dropped,
                    reason: "  ".to_owned(),
                    components: Vec::new(),
                },
            ],
        );
        assert!(
            silent
                .validate_success(Operation::Generate, &active)
                .is_err()
        );

        // A rejection fails the compile, whatever else the report says.
        let rejected = CompileReport::new(
            Operation::Generate,
            vec![
                native,
                Decision {
                    field: FieldId::GenerateContextRole,
                    disposition: Disposition::Rejected,
                    reason: "cannot express".to_owned(),
                    components: Vec::new(),
                },
            ],
        );
        assert!(
            rejected
                .validate_success(Operation::Generate, &active)
                .is_err()
        );

        // The wrong operation is a compiler bug, not a request problem.
        assert!(good.validate_success(Operation::Embed, &active).is_err());
    }

    #[test]
    fn an_operation_round_trips_and_refuses_what_it_does_not_know() {
        for operation in Operation::ALL {
            assert_eq!(
                Operation::parse(operation.as_str()).expect("round trip"),
                *operation
            );
        }
        assert!(Operation::parse("rerank").is_err());
        assert_eq!(Operation::Transcription.as_str(), "transcription");
    }
}
