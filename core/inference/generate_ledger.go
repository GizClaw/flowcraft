package inference

import (
	"github.com/GizClaw/flowcraft/core/message"
)

// Content parts activate one ledger field per kind and position. Both tables
// are keyed by kind and walked in message.PartKinds order, so the field list of
// a request is deterministic and adding a content kind is one row per position.
// TestGenerateLedgerCoversPartKinds pins both tables against the vocabulary.
var (
	generateContextPartFields = map[message.PartKind]FieldID{
		message.PartText:       FieldGenerateContextText,
		message.PartImage:      FieldGenerateContextImage,
		message.PartAudio:      FieldGenerateContextAudio,
		message.PartVideo:      FieldGenerateContextVideo,
		message.PartFile:       FieldGenerateContextFile,
		message.PartData:       FieldGenerateContextData,
		message.PartToolCall:   FieldGenerateContextToolCall,
		message.PartToolResult: FieldGenerateContextToolResult,
		message.PartReasoning:  FieldGenerateContextReasoning,
	}
	generateInputPartFields = map[message.PartKind]FieldID{
		message.PartText:       FieldGenerateInputText,
		message.PartImage:      FieldGenerateInputImage,
		message.PartAudio:      FieldGenerateInputAudio,
		message.PartVideo:      FieldGenerateInputVideo,
		message.PartFile:       FieldGenerateInputFile,
		message.PartData:       FieldGenerateInputData,
		message.PartToolCall:   FieldGenerateInputToolCall,
		message.PartToolResult: FieldGenerateInputToolResult,
		message.PartReasoning:  FieldGenerateInputReasoning,
	}
)

// GenerateContextPartField returns the ledger field a context turn activates
// for one content kind.
//
// The boolean is false when the vocabulary names a kind this ledger has no row
// for. That is vocabulary drift — core's part list and its field table
// disagree — not an unsupported request: the request-side answer for a kind a
// model cannot serve is a rejection, and the driver still has the field to name
// in it. TestGenerateLedgerCoversPartKinds keeps this from happening, so a
// false here is a bug to report rather than a case to recover from.
func GenerateContextPartField(kind message.PartKind) (FieldID, bool) {
	field, ok := generateContextPartFields[kind]
	return field, ok
}

// GenerateInputPartField returns the ledger field the current input activates
// for one content kind. See GenerateContextPartField for what false means.
func GenerateInputPartField(kind message.PartKind) (FieldID, bool) {
	field, ok := generateInputPartFields[kind]
	return field, ok
}

func appendGenerateContextPartFields(
	fields []FieldID,
	messages []message.Message,
) []FieldID {
	kinds := make(map[message.PartKind]bool)
	for _, msg := range messages {
		for _, part := range msg.Content.Parts {
			if part != nil {
				kinds[part.Kind()] = true
			}
		}
	}
	return appendPartFields(fields, generateContextPartFields, kinds)
}

func appendGenerateInputPartFields(fields []FieldID, parts []message.Part) []FieldID {
	kinds := make(map[message.PartKind]bool, len(parts))
	for _, part := range parts {
		if part != nil {
			kinds[part.Kind()] = true
		}
	}
	return appendPartFields(fields, generateInputPartFields, kinds)
}

// appendPartFields appends the field of every present kind, in vocabulary
// order. A kind without a row contributes nothing; the table coverage test is
// what keeps that from happening unnoticed.
func appendPartFields(
	fields []FieldID,
	table map[message.PartKind]FieldID,
	present map[message.PartKind]bool,
) []FieldID {
	for _, kind := range message.PartKinds() {
		if !present[kind] {
			continue
		}
		field, ok := table[kind]
		if !ok {
			continue
		}
		fields = append(fields, field)
	}
	return fields
}

func appendGenerateIntentFields(fields []FieldID, intent Intent) []FieldID {
	if intent.Text != nil {
		fields = append(fields, FieldGenerateIntentText)
		if intent.Text.Response != nil {
			fields = append(fields, FieldGenerateIntentTextResponse)
			if intent.Text.Response.Kind != "" {
				fields = append(fields, FieldGenerateIntentTextResponseKind)
			}
			if intent.Text.Response.Name != "" {
				fields = append(fields, FieldGenerateIntentTextResponseName)
			}
			if len(intent.Text.Response.Schema) > 0 {
				fields = append(fields, FieldGenerateIntentTextResponseSchema)
			}
		}
		if intent.Text.MaxOutputTokens != nil {
			fields = append(fields, FieldGenerateIntentTextMaxOutputTokens)
		}
		if len(intent.Text.Tools) > 0 {
			fields = append(fields, FieldGenerateIntentTools)
		}
		if intent.Text.ToolChoice != nil {
			fields = append(fields, FieldGenerateIntentToolChoice)
			if intent.Text.ToolChoice.Kind != "" {
				fields = append(fields, FieldGenerateIntentToolChoiceKind)
			}
			if intent.Text.ToolChoice.Name != "" {
				fields = append(fields, FieldGenerateIntentToolChoiceName)
			}
		}
		if intent.Text.Temperature != nil {
			fields = append(fields, FieldGenerateIntentTemperature)
		}
		if intent.Text.TopP != nil {
			fields = append(fields, FieldGenerateIntentTopP)
		}
		if intent.Text.ReasoningEnabled != nil || intent.Text.ReasoningEffort != "" {
			fields = append(fields, FieldGenerateIntentReasoning)
		}
		if intent.Text.ReasoningEnabled != nil {
			fields = append(fields, FieldGenerateIntentReasoningEnabled)
		}
		if intent.Text.ReasoningEffort != "" {
			fields = append(fields, FieldGenerateIntentReasoningEffort)
		}
	}
	if intent.Image != nil {
		fields = append(fields, FieldGenerateIntentImage)
		if intent.Image.Size != nil {
			fields = append(
				fields,
				FieldGenerateIntentImageSize,
				FieldGenerateIntentImageSizeWidth,
				FieldGenerateIntentImageSizeHeight,
			)
		}
		if intent.Image.AspectRatio != "" {
			fields = append(fields, FieldGenerateIntentImageAspectRatio)
		}
		if intent.Image.Count != nil {
			fields = append(fields, FieldGenerateIntentImageCount)
		}
		if intent.Image.Seed != nil {
			fields = append(fields, FieldGenerateIntentImageSeed)
		}
		if intent.Image.OutputFormat != "" {
			fields = append(fields, FieldGenerateIntentImageOutputFormat)
		}
		if intent.Image.Delivery != "" {
			fields = append(fields, FieldGenerateIntentImageDelivery)
		}
		if intent.Image.Quality != "" {
			fields = append(fields, FieldGenerateIntentImageQuality)
		}
	}
	if intent.Audio != nil {
		fields = append(fields, FieldGenerateIntentAudio, FieldGenerateIntentAudioVoice)
		if intent.Audio.Voice.ID != "" {
			fields = append(fields, FieldGenerateIntentAudioVoiceID)
		}
		if intent.Audio.Voice.Language != "" {
			fields = append(fields, FieldGenerateIntentAudioVoiceLanguage)
		}
		fields = append(fields, FieldGenerateIntentAudioFormat)
		if intent.Audio.Format.Encoding != "" {
			fields = append(fields, FieldGenerateIntentAudioFormatEncoding)
		}
		if intent.Audio.Format.SampleRateHz != 0 {
			fields = append(fields, FieldGenerateIntentAudioFormatSampleRate)
		}
		if intent.Audio.Format.Channels != 0 {
			fields = append(fields, FieldGenerateIntentAudioFormatChannels)
		}
		if intent.Audio.Speed != nil {
			fields = append(fields, FieldGenerateIntentAudioSpeed)
		}
		if intent.Audio.Count != nil {
			fields = append(fields, FieldGenerateIntentAudioCount)
		}
	}
	if intent.Video != nil {
		fields = append(fields, FieldGenerateIntentVideo)
		if intent.Video.DurationMillis != nil {
			fields = append(fields, FieldGenerateIntentVideoDuration)
		}
		if intent.Video.Resolution != "" {
			fields = append(fields, FieldGenerateIntentVideoResolution)
		}
		if intent.Video.AspectRatio != "" {
			fields = append(fields, FieldGenerateIntentVideoAspectRatio)
		}
		if intent.Video.Seed != nil {
			fields = append(fields, FieldGenerateIntentVideoSeed)
		}
		if intent.Video.Watermark != nil {
			fields = append(fields, FieldGenerateIntentVideoWatermark)
		}
	}
	return fields
}
