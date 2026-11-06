package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/openai/openai-go/v3"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
)

// TranscriptionExtensionID names the transcription options extension.
const TranscriptionExtensionID = "transcription"

// TranscriptMediaType is the media type of the structured part a transcript
// carries when the caller asked for timed segments. The schema is documented
// here, next to the driver that produces it, because it is this driver's
// payload — the inference contract only says a data part is a JSON object with a
// media type.
const TranscriptMediaType = "application/vnd.flowcraft.transcript+json"

// TranscriptionOptions configures one speech-to-text call. Language and prompt
// are provider parameters, not provider-agnostic request fields, so they ride
// this extension rather than a JSON field on every generate request.
type TranscriptionOptions struct {
	// Language is an ISO-639-1 hint. Empty means "detect".
	Language string `json:"language,omitempty" yaml:"language,omitempty"`
	// Prompt is vocabulary or context for the transcriber.
	Prompt string `json:"prompt,omitempty" yaml:"prompt,omitempty"`
	// Segments asks for timed segments. Only models that support timestamp
	// granularities can serve it; the others report a warning rather than an
	// empty slice.
	Segments bool `json:"segments,omitempty" yaml:"segments,omitempty"`
	// Temperature tunes decoding. Zero means the provider's default.
	Temperature *float64 `json:"temperature,omitempty" yaml:"temperature,omitempty"`
}

// ProviderID implements [inference.Extension].
func (*TranscriptionOptions) ProviderID() string { return ProviderID }

// ExtensionID implements [inference.Extension].
func (*TranscriptionOptions) ExtensionID() string { return TranscriptionExtensionID }

// ActiveFields lists the options this value actually sets.
func (o *TranscriptionOptions) ActiveFields() []inference.ExtensionField {
	if o == nil {
		return nil
	}
	var fields []inference.ExtensionField
	add := func(name string) { fields = append(fields, inference.ExtensionField(name)) }
	if o.Language != "" {
		add("language")
	}
	if o.Prompt != "" {
		add("prompt")
	}
	if o.Segments {
		add("segments")
	}
	if o.Temperature != nil {
		add("temperature")
	}
	return fields
}

// Validate checks the options that have a closed value set.
func (o *TranscriptionOptions) Validate() error {
	if o == nil {
		return nil
	}
	if o.Temperature != nil && (*o.Temperature < 0 || *o.Temperature > 1) {
		return fmt.Errorf("transcription temperature must be within [0, 1]")
	}
	return nil
}

// Clone returns an independent copy.
func (o *TranscriptionOptions) Clone() inference.Extension {
	if o == nil {
		return (*TranscriptionOptions)(nil)
	}
	clone := *o
	if o.Temperature != nil {
		value := *o.Temperature
		clone.Temperature = &value
	}
	return &clone
}

// decodeTranscription is the config-side decoder for the transcription options.
func decodeTranscription(fields json.RawMessage) (inference.Extension, error) {
	options := &TranscriptionOptions{}
	if len(fields) > 0 {
		if err := inference.DecodeStrict(fields, options); err != nil {
			return nil, err
		}
	}
	if err := options.Validate(); err != nil {
		return nil, err
	}
	return options, nil
}

// transcriptionOptions extracts this driver's transcription extension.
func transcriptionOptions(extensions inference.Extensions) *TranscriptionOptions {
	for _, extension := range extensions {
		if options, ok := extension.(*TranscriptionOptions); ok {
			return options
		}
	}
	return nil
}

// hasAudioInput reports whether the turn carries audio as its input. That is
// what selects transcription: the caller asked for text, and the only surface
// that turns audio into text is the transcription endpoint. Context is
// deliberately ignored — audio in a conversation is a different feature and this
// driver does not serve it yet.
func hasAudioInput(request inference.GenerateRequest) bool {
	for _, part := range request.Input.Content.Parts {
		if part.Kind() == message.PartAudio {
			return true
		}
	}
	return false
}

// transcriptionAudioFile is the multipart payload: the endpoint needs a
// filename, and the extension is how it decides the container format.
type transcriptionAudioFile struct {
	reader *bytes.Reader
	name   string
}

func (f transcriptionAudioFile) Read(p []byte) (int, error) { return f.reader.Read(p) }

// filenameForAudio maps a source media type onto the extension the endpoint
// expects. An unknown type is rejected rather than guessed, because a wrong
// extension is a wrong container format.
func filenameForAudio(mediaType string) (string, error) {
	switch mediaType {
	case "audio/mpeg", "audio/mp3":
		return "audio.mp3", nil
	case "audio/wav", "audio/x-wav":
		return "audio.wav", nil
	case "audio/webm":
		return "audio.webm", nil
	case "audio/mp4", "audio/m4a", "audio/x-m4a":
		return "audio.m4a", nil
	case "audio/flac", "audio/x-flac":
		return "audio.flac", nil
	case "audio/ogg", "audio/opus":
		return "audio.ogg", nil
	default:
		return "", inference.NewError(
			inference.UnsupportedFeature,
			inference.FieldInput,
			fmt.Errorf("openai: transcription cannot send audio media type %q", mediaType),
		)
	}
}

// transcribeCompile maps a generate request onto transcription parameters.
func (d *driver) transcribeCompile(
	_ context.Context,
	request inference.GenerateRequest,
	shape protocol.Shape,
) (protocol.Compiled[transcriptionWire], error) {
	if shape != protocol.ShapeUnary {
		return protocol.Compiled[transcriptionWire]{}, inference.NewError(
			inference.UnsupportedFeature,
			inference.FieldStream,
			errors.New("openai: streaming transcription is not implemented"),
		)
	}
	if request.Output.Kind() != inference.OutputText {
		return protocol.Compiled[transcriptionWire]{}, inference.NewError(
			inference.UnsupportedFeature,
			inference.FieldOutput,
			fmt.Errorf("openai: transcription serves text output, not %q", request.Output.Kind()),
		)
	}
	if len(request.Context) > 0 {
		return protocol.Compiled[transcriptionWire]{}, inference.NewError(
			inference.UnsupportedFeature,
			inference.FieldContext,
			errors.New("openai: transcription has no conversation context; put the audio in the input"),
		)
	}
	if err := d.checkDeclarations(request); err != nil {
		return protocol.Compiled[transcriptionWire]{}, err
	}

	var audio message.AudioPart
	found := 0
	for _, part := range request.Input.Content.Parts {
		switch typed := part.(type) {
		case message.AudioPart:
			audio = typed
			found++
		default:
			return protocol.Compiled[transcriptionWire]{}, inference.NewError(
				inference.UnsupportedFeature,
				inference.FieldInput,
				fmt.Errorf("openai: transcription takes audio input only, not %q", part.Kind()),
			)
		}
	}
	if found != 1 {
		return protocol.Compiled[transcriptionWire]{}, inference.NewError(
			inference.InvalidRequest,
			inference.FieldInput,
			errors.New("openai: transcription requires exactly one audio part"),
		)
	}

	data := audio.Source.Bytes()
	if len(data) == 0 {
		return protocol.Compiled[transcriptionWire]{}, inference.NewError(
			inference.InvalidRequest,
			inference.FieldInput,
			errors.New("openai: transcription requires audio bytes; URL sources are not fetched by this driver"),
		)
	}
	name, err := filenameForAudio(audio.Source.MediaType())
	if err != nil {
		return protocol.Compiled[transcriptionWire]{}, err
	}

	params := openai.AudioTranscriptionNewParams{
		File:           transcriptionAudioFile{reader: bytes.NewReader(data), name: name},
		Model:          openai.AudioModel(request.Model.ID.Name),
		ResponseFormat: openai.AudioResponseFormatVerboseJSON,
	}

	options := transcriptionOptions(request.Extensions)
	if options != nil {
		if options.Language != "" {
			params.Language = openai.String(options.Language)
		}
		if options.Prompt != "" {
			params.Prompt = openai.String(options.Prompt)
		}
		if options.Temperature != nil {
			params.Temperature = openai.Float(*options.Temperature)
		}
		if options.Segments {
			params.TimestampGranularities = []string{"segment"}
		}
	}

	fields := append(honoredFields(request), qualifiedFields(options)...)
	return protocol.Compiled[transcriptionWire]{
		Wire:   transcriptionWire{params: params, segments: options != nil && options.Segments},
		Fields: fields,
	}, nil
}

// transcriptionWire carries the request parameters plus the one fact the decoder
// needs and the wire does not echo back: whether segments were requested.
type transcriptionWire struct {
	params   openai.AudioTranscriptionNewParams
	segments bool
}

// transcriptionPayload is the verbose-json answer.
type transcriptionPayload struct {
	Text     string  `json:"text"`
	Language string  `json:"language"`
	Duration float64 `json:"duration"`
	Segments []struct {
		Start float64 `json:"start"`
		End   float64 `json:"end"`
		Text  string  `json:"text"`
	} `json:"segments"`
}

// transcribeCall performs the provider I/O.
func (d *driver) transcribeCall(
	ctx context.Context,
	_ inference.GenerateRequest,
	wire transcriptionWire,
) (transcriptionPayload, error) {
	response, err := d.client.Audio.Transcriptions.New(ctx, wire.params)
	if err != nil {
		return transcriptionPayload{}, err
	}
	payload := transcriptionPayload{
		Text:     response.Text,
		Language: response.Language,
		Duration: response.Duration,
	}
	for _, segment := range response.Segments {
		payload.Segments = append(payload.Segments, struct {
			Start float64 `json:"start"`
			End   float64 `json:"end"`
			Text  string  `json:"text"`
		}{Start: segment.Start, End: segment.End, Text: segment.Text})
	}
	if payload.Text == "" && len(payload.Segments) == 0 {
		return transcriptionPayload{}, inference.NewError(
			inference.ProviderResponseUnusable,
			inference.FieldContent,
			errors.New("openai: transcription response carried no text"),
		)
	}
	return payload, nil
}

// transcribeDecode wraps the transcript in an assistant message.
func (d *driver) transcribeDecode(
	_ context.Context,
	request inference.GenerateRequest,
	raw transcriptionPayload,
) (inference.GenerateResponse, error) {
	parts := []message.Part{message.TextPart{Text: raw.Text}}
	warnings := []inference.Warning{}

	options := transcriptionOptions(request.Extensions)
	wantsSegments := options != nil && options.Segments
	if wantsSegments {
		if len(raw.Segments) == 0 {
			warnings = append(warnings, inference.Warning{
				Field:   inference.Field(fmt.Sprintf("extension.%s.%s.segments", ProviderID, TranscriptionExtensionID)),
				Code:    "unsupported",
				Message: "openai: this transcription model returned no timed segments",
			})
		} else {
			// A data part's value must be a JSON object (the contract rejects a
			// bare array), so the segments ride under a named key.
			value, err := json.Marshal(map[string]any{"segments": raw.Segments})
			if err != nil {
				return inference.GenerateResponse{}, err
			}
			parts = append(parts, message.DataPart{MediaType: TranscriptMediaType, Value: value})
		}
	}

	var millis *int64
	if raw.Duration > 0 {
		value := int64(math.Round(raw.Duration * float64(time.Second/time.Millisecond)))
		millis = &value
	}

	return inference.GenerateResponse{
		Message: message.Message{
			Role:    message.RoleAssistant,
			Content: message.Content{Parts: parts},
		},
		FinishReason: inference.FinishStop,
		Usage:        usage.Usage{AudioDurationMillis: millis},
		Metadata: inference.Metadata{
			Model: model.ModelID{Provider: ProviderID, Name: request.Model.ID.Name},
		},
		Warnings: warnings,
	}, nil
}
