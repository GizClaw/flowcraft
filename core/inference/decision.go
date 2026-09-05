package inference

import (
	"fmt"
)

type FieldID string

const (
	FieldGenerateExecutionUnary              FieldID = "generate.execution.unary"
	FieldGenerateExecutionStream             FieldID = "generate.execution.stream"
	FieldGenerateContextRole                 FieldID = "generate.context.*.role"
	FieldGenerateContextText                 FieldID = "generate.context.*.content.parts.text"
	FieldGenerateContextImage                FieldID = "generate.context.*.content.parts.image"
	FieldGenerateContextAudio                FieldID = "generate.context.*.content.parts.audio"
	FieldGenerateContextVideo                FieldID = "generate.context.*.content.parts.video"
	FieldGenerateContextFile                 FieldID = "generate.context.*.content.parts.file"
	FieldGenerateContextData                 FieldID = "generate.context.*.content.parts.data"
	FieldGenerateContextToolCall             FieldID = "generate.context.*.content.parts.tool_call"
	FieldGenerateContextToolResult           FieldID = "generate.context.*.content.parts.tool_result"
	FieldGenerateContextReasoning            FieldID = "generate.context.*.content.parts.reasoning"
	FieldGenerateInputRole                   FieldID = "generate.input.role"
	FieldGenerateInputText                   FieldID = "generate.input.content.parts.text"
	FieldGenerateInputImage                  FieldID = "generate.input.content.parts.image"
	FieldGenerateInputAudio                  FieldID = "generate.input.content.parts.audio"
	FieldGenerateInputVideo                  FieldID = "generate.input.content.parts.video"
	FieldGenerateInputFile                   FieldID = "generate.input.content.parts.file"
	FieldGenerateInputData                   FieldID = "generate.input.content.parts.data"
	FieldGenerateInputToolCall               FieldID = "generate.input.content.parts.tool_call"
	FieldGenerateInputToolResult             FieldID = "generate.input.content.parts.tool_result"
	FieldGenerateInputReasoning              FieldID = "generate.input.content.parts.reasoning"
	FieldGenerateIntentText                  FieldID = "generate.input.content.intent.text"
	FieldGenerateIntentTextResponse          FieldID = "generate.input.content.intent.text.response"
	FieldGenerateIntentTextResponseKind      FieldID = "generate.input.content.intent.text.response.kind"
	FieldGenerateIntentTextResponseName      FieldID = "generate.input.content.intent.text.response.name"
	FieldGenerateIntentTextResponseSchema    FieldID = "generate.input.content.intent.text.response.schema"
	FieldGenerateIntentTextMaxOutputTokens   FieldID = "generate.input.content.intent.text.max_output_tokens"
	FieldGenerateIntentImage                 FieldID = "generate.input.content.intent.image"
	FieldGenerateIntentImageSize             FieldID = "generate.input.content.intent.image.size"
	FieldGenerateIntentImageSizeWidth        FieldID = "generate.input.content.intent.image.size.width"
	FieldGenerateIntentImageSizeHeight       FieldID = "generate.input.content.intent.image.size.height"
	FieldGenerateIntentImageAspectRatio      FieldID = "generate.input.content.intent.image.aspect_ratio"
	FieldGenerateIntentImageCount            FieldID = "generate.input.content.intent.image.count"
	FieldGenerateIntentImageSeed             FieldID = "generate.input.content.intent.image.seed"
	FieldGenerateIntentImageOutputFormat     FieldID = "generate.input.content.intent.image.output_format"
	FieldGenerateIntentImageDelivery         FieldID = "generate.input.content.intent.image.delivery"
	FieldGenerateIntentImageQuality          FieldID = "generate.input.content.intent.image.quality"
	FieldGenerateIntentAudio                 FieldID = "generate.input.content.intent.audio"
	FieldGenerateIntentAudioVoice            FieldID = "generate.input.content.intent.audio.voice"
	FieldGenerateIntentAudioVoiceID          FieldID = "generate.input.content.intent.audio.voice.id"
	FieldGenerateIntentAudioVoiceLanguage    FieldID = "generate.input.content.intent.audio.voice.language"
	FieldGenerateIntentAudioFormat           FieldID = "generate.input.content.intent.audio.format"
	FieldGenerateIntentAudioFormatEncoding   FieldID = "generate.input.content.intent.audio.format.encoding"
	FieldGenerateIntentAudioFormatSampleRate FieldID = "generate.input.content.intent.audio.format.sample_rate_hz"
	FieldGenerateIntentAudioFormatChannels   FieldID = "generate.input.content.intent.audio.format.channels"
	FieldGenerateIntentAudioSpeed            FieldID = "generate.input.content.intent.audio.speed"
	FieldGenerateIntentAudioCount            FieldID = "generate.input.content.intent.audio.count"
	FieldGenerateIntentVideo                 FieldID = "generate.input.content.intent.video"
	FieldGenerateIntentVideoDuration         FieldID = "generate.input.content.intent.video.duration_millis"
	FieldGenerateIntentVideoResolution       FieldID = "generate.input.content.intent.video.resolution"
	FieldGenerateIntentVideoAspectRatio      FieldID = "generate.input.content.intent.video.aspect_ratio"
	FieldGenerateIntentVideoSeed             FieldID = "generate.input.content.intent.video.seed"
	FieldGenerateIntentVideoWatermark        FieldID = "generate.input.content.intent.video.watermark"
	FieldGenerateIntentTools                 FieldID = "generate.input.content.intent.text.tools"
	FieldGenerateIntentToolChoice            FieldID = "generate.input.content.intent.text.tool_choice"
	FieldGenerateIntentToolChoiceKind        FieldID = "generate.input.content.intent.text.tool_choice.kind"
	FieldGenerateIntentToolChoiceName        FieldID = "generate.input.content.intent.text.tool_choice.name"
	FieldGenerateIntentTemperature           FieldID = "generate.input.content.intent.text.temperature"
	FieldGenerateIntentTopP                  FieldID = "generate.input.content.intent.text.top_p"
	FieldGenerateIntentReasoning             FieldID = "generate.input.content.intent.text.reasoning"
	FieldGenerateIntentReasoningEnabled      FieldID = "generate.input.content.intent.text.reasoning_enabled"
	FieldGenerateIntentReasoningEffort       FieldID = "generate.input.content.intent.text.reasoning_effort"
	FieldGenerateRequestMetadata             FieldID = "generate.request.metadata"
	FieldEmbedItems                          FieldID = "embed.items"
	FieldEmbedItemText                       FieldID = "embed.items.content.text"
	FieldEmbedItemImage                      FieldID = "embed.items.content.image"
	FieldEmbedItemAudio                      FieldID = "embed.items.content.audio"
	FieldEmbedItemVideo                      FieldID = "embed.items.content.video"
	FieldEmbedItemFile                       FieldID = "embed.items.content.file"
	FieldEmbedItemData                       FieldID = "embed.items.content.data"
	FieldEmbedItemToolCall                   FieldID = "embed.items.content.tool_call"
	FieldEmbedItemToolResult                 FieldID = "embed.items.content.tool_result"
	FieldEmbedItemMultiPart                  FieldID = "embed.items.content.multi_part"
	FieldEmbedDimensions                     FieldID = "embed.dimensions"
	FieldTranscriptionAudio                  FieldID = "transcription.audio"
	FieldTranscriptionLanguage               FieldID = "transcription.language"
	FieldTranscriptionPrompt                 FieldID = "transcription.prompt"
	FieldTranscriptionTimestamps             FieldID = "transcription.timestamps"
	FieldTranscriptionInputFormat            FieldID = "transcription.session.input_format"
	FieldRealtimeInstructions                FieldID = "realtime.instructions"
	FieldRealtimeModalities                  FieldID = "realtime.modalities"
	FieldRealtimeInputAudioFormat            FieldID = "realtime.input_audio_format"
	FieldRealtimeOutputAudioFormat           FieldID = "realtime.output_audio_format"
	FieldRealtimeVoice                       FieldID = "realtime.voice"
	FieldRealtimeTools                       FieldID = "realtime.tools"
	FieldRealtimeInputText                   FieldID = "realtime.input.text"
	FieldRealtimeInputAudio                  FieldID = "realtime.input.audio"
	FieldRealtimeInputVideo                  FieldID = "realtime.input.video"
	FieldRealtimeInputToolResult             FieldID = "realtime.input.tool_result"
)

type Disposition string

const (
	// Native means the compiler consumed the field into the provider wire.
	Native Disposition = "native"
	// Rejected means the field cannot be honored and the compile failed.
	Rejected Disposition = "rejected"
	// Dropped means the compiler intentionally discarded the field: the
	// value cannot round-trip or has no channel, and the Reason states why.
	// Drops are field-level audits and may appear on a successful compile
	// or alongside a rejection on a failed one; consumers audit them
	// through the report, so nothing is discarded silently.
	Dropped Disposition = "dropped"
)

type Decision struct {
	Field       FieldID     `json:"field"`
	Disposition Disposition `json:"disposition"`
	Reason      string      `json:"reason,omitempty"`
}

type CompileReport struct {
	Operation Operation  `json:"operation"`
	Decisions []Decision `json:"decisions"`
}

func (r CompileReport) Clone() CompileReport {
	r.Decisions = append([]Decision(nil), r.Decisions...)
	return r
}

func (r CompileReport) Rejects(field FieldID) bool {
	for _, decision := range r.Decisions {
		if decision.Field == field && decision.Disposition == Rejected {
			return true
		}
	}
	return false
}

// Dropped reports whether the field was dropped with a reason while the
// compile otherwise succeeded.
func (r CompileReport) Dropped(field FieldID) bool {
	for _, decision := range r.Decisions {
		if decision.Field == field && decision.Disposition == Dropped {
			return true
		}
	}
	return false
}

func (r CompileReport) Metadata(model ModelRef) Metadata {
	return Metadata{
		Model:     model.ID,
		Operation: r.Operation,
		Decisions: append([]Decision(nil), r.Decisions...),
	}
}

// ValidateSuccess proves that a successful compile covers every active
// canonical field with exactly one terminal disposition: Native, or Dropped
// with a reason. Rejections invalidate a successful report.
func (r CompileReport) ValidateSuccess(operation Operation, active []FieldID) error {
	if r.Operation != operation {
		return contractViolation(operation, "", "compiler reported the wrong operation")
	}
	activeSet := make(map[FieldID]struct{}, len(active))
	for _, field := range active {
		activeSet[field] = struct{}{}
	}
	seen := make(map[FieldID]struct{}, len(r.Decisions))
	for _, decision := range r.Decisions {
		if _, ok := activeSet[decision.Field]; !ok {
			return contractViolation(r.Operation, decision.Field, "decision covers an inactive field")
		}
		if _, ok := seen[decision.Field]; ok {
			return contractViolation(r.Operation, decision.Field, "duplicate field disposition")
		}
		seen[decision.Field] = struct{}{}
		switch decision.Disposition {
		case Native:
		case Dropped:
			if decision.Reason == "" {
				return contractViolation(r.Operation, decision.Field, "dropped field carries no reason")
			}
		case Rejected:
			return contractViolation(r.Operation, decision.Field, "successful compile contains rejection")
		default:
			return contractViolation(r.Operation, decision.Field, "unknown disposition")
		}
	}
	for _, field := range active {
		if _, ok := seen[field]; !ok {
			return contractViolation(r.Operation, field, "active field has no disposition")
		}
	}
	return nil
}

// ValidateFailure proves that a failed compile rejects at least one active
// field and contains no duplicate or out-of-ledger decisions. Dropped
// decisions are field-level audits: a driver may both reject one field
// and drop another (for example an image input rejected by a text model
// while the reasoning effort folds onto the model's wire level), so a
// valid dropped disposition with a reason does not invalidate the report.
func (r CompileReport) ValidateFailure(operation Operation, active []FieldID) error {
	if r.Operation != operation {
		return contractViolation(operation, "", "compiler reported the wrong operation")
	}
	activeSet := make(map[FieldID]struct{}, len(active))
	for _, field := range active {
		activeSet[field] = struct{}{}
	}
	seen := make(map[FieldID]struct{}, len(r.Decisions))
	rejected := false
	for _, decision := range r.Decisions {
		if _, ok := activeSet[decision.Field]; !ok {
			return contractViolation(operation, decision.Field, "decision covers an inactive field")
		}
		if _, ok := seen[decision.Field]; ok {
			return contractViolation(operation, decision.Field, "duplicate field disposition")
		}
		seen[decision.Field] = struct{}{}
		switch decision.Disposition {
		case Native:
		case Dropped:
			if decision.Reason == "" {
				return contractViolation(operation, decision.Field, "dropped field carries no reason")
			}
		case Rejected:
			if decision.Reason == "" {
				return contractViolation(operation, decision.Field, "rejected disposition requires a reason")
			}
			rejected = true
		default:
			return contractViolation(operation, decision.Field, "unknown disposition")
		}
	}
	if !rejected {
		return contractViolation(operation, "", "failed compile has no rejected field")
	}
	return nil
}

func contractViolation(operation Operation, field FieldID, message string) error {
	return NewError(
		CompilerContractViolation,
		operation,
		field,
		fmt.Errorf("%s", message),
	)
}
