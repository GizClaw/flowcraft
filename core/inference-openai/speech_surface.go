package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/openai/openai-go/v3"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// SpeechExtensionID names the speech options extension. Voice, format and speed
// share nothing with the text and image option sets, so they get their own.
const SpeechExtensionID = "speech"

// SpeechOptions configures one speech synthesis call.
type SpeechOptions struct {
	// Voice is the provider's voice name, for example "alloy" or "nova".
	Voice string `json:"voice,omitempty" yaml:"voice,omitempty"`
	// Format is the encoded audio format: mp3, opus, aac, flac, wav or pcm.
	// Empty keeps the provider's default, which is mp3.
	Format string `json:"format,omitempty" yaml:"format,omitempty"`
	// Speed scales the speaking rate.
	Speed *float64 `json:"speed,omitempty" yaml:"speed,omitempty"`
	// Instructions steers delivery (tone, pacing) on models that accept it.
	Instructions string `json:"instructions,omitempty" yaml:"instructions,omitempty"`
}

// ProviderID implements [inference.Extension].
func (*SpeechOptions) ProviderID() string { return ProviderID }

// ExtensionID implements [inference.Extension].
func (*SpeechOptions) ExtensionID() string { return SpeechExtensionID }

// ActiveFields lists the options this value actually sets.
func (o *SpeechOptions) ActiveFields() []inference.ExtensionField {
	if o == nil {
		return nil
	}
	var fields []inference.ExtensionField
	add := func(name string) { fields = append(fields, inference.ExtensionField(name)) }
	if o.Voice != "" {
		add("voice")
	}
	if o.Format != "" {
		add("format")
	}
	if o.Speed != nil {
		add("speed")
	}
	if o.Instructions != "" {
		add("instructions")
	}
	return fields
}

// Validate checks the options that have a closed value set.
func (o *SpeechOptions) Validate() error {
	if o == nil {
		return nil
	}
	switch o.Format {
	case "", "mp3", "opus", "aac", "flac", "wav", "pcm":
	default:
		return fmt.Errorf("unknown speech format %q", o.Format)
	}
	if o.Speed != nil && (*o.Speed < 0.25 || *o.Speed > 4) {
		return fmt.Errorf("speed must be within [0.25, 4]")
	}
	return nil
}

// Clone returns an independent copy.
func (o *SpeechOptions) Clone() inference.Extension {
	if o == nil {
		return (*SpeechOptions)(nil)
	}
	clone := *o
	if o.Speed != nil {
		value := *o.Speed
		clone.Speed = &value
	}
	return &clone
}

// decodeSpeech is the config-side decoder for the speech options extension.
func decodeSpeech(fields json.RawMessage) (inference.Extension, error) {
	options := &SpeechOptions{}
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

// speechOptions extracts this driver's speech extension from a request.
func speechOptions(extensions inference.Extensions) *SpeechOptions {
	for _, extension := range extensions {
		if options, ok := extension.(*SpeechOptions); ok {
			return options
		}
	}
	return nil
}

// speechCompile maps a request onto speech synthesis parameters. The streaming
// shape asks for the raw-audio stream format, which the caller consumes as
// chunks.
func (d *driver) speechCompile(
	_ context.Context,
	request inference.GenerateRequest,
	shape protocol.Shape,
) (protocol.Compiled[openai.AudioSpeechNewParams], error) {
	if request.Output.Kind() != inference.OutputAudio {
		return protocol.Compiled[openai.AudioSpeechNewParams]{}, inference.NewError(
			inference.UnsupportedFeature,
			inference.FieldOutput,
			fmt.Errorf("openai: speech pipeline cannot serve output %q", request.Output.Kind()),
		)
	}
	if len(request.Context) > 0 {
		return protocol.Compiled[openai.AudioSpeechNewParams]{}, inference.NewError(
			inference.UnsupportedFeature,
			inference.FieldContext,
			fmt.Errorf("openai: speech synthesis has no conversation context; put the text in the input"),
		)
	}
	if err := d.checkDeclarations(request); err != nil {
		return protocol.Compiled[openai.AudioSpeechNewParams]{}, err
	}

	options := speechOptions(request.Extensions)
	speech, err := speechInput(request.Input.Message())
	if err != nil {
		return protocol.Compiled[openai.AudioSpeechNewParams]{}, err
	}
	params := openai.AudioSpeechNewParams{
		Model: openai.SpeechModel(request.Model.ID.Name),
		Input: speech,
	}
	if options != nil {
		if options.Voice != "" {
			params.Voice = openai.AudioSpeechNewParamsVoiceUnion{OfString: openai.String(options.Voice)}
		}
		if options.Format != "" {
			params.ResponseFormat = openai.AudioSpeechNewParamsResponseFormat(options.Format)
		}
		if options.Speed != nil {
			params.Speed = openai.Float(*options.Speed)
		}
		if options.Instructions != "" {
			params.Instructions = openai.String(options.Instructions)
		}
	}

	if shape == protocol.ShapeStream {
		// The raw-audio stream format is what makes chunked delivery possible; the
		// sse format is not supported by every model this driver serves.
		params.StreamFormat = openai.AudioSpeechNewParamsStreamFormatAudio
	}
	fields := append(honoredFields(request), qualifiedFields(options)...)
	return protocol.Compiled[openai.AudioSpeechNewParams]{
		Wire:   params,
		Fields: fields,
	}, nil
}

// speechInput joins the input's text. Speech synthesis speaks text, so any other
// content kind is an error rather than a silent omission.
func speechInput(item message.Message) (string, error) {
	texts := make([]string, 0, len(item.Content.Parts))
	for _, part := range item.Content.Parts {
		text, ok := part.(message.TextPart)
		if !ok {
			return "", inference.NewError(
				inference.UnsupportedFeature,
				inference.FieldContent,
				fmt.Errorf("openai: speech synthesis takes text input only, not %q", part.Kind()),
			)
		}
		texts = append(texts, text.Text)
	}
	joined := strings.TrimSpace(strings.Join(texts, "\n"))
	if joined == "" {
		return "", inference.NewError(
			inference.InvalidRequest,
			inference.FieldContent,
			fmt.Errorf("openai: speech synthesis requires text"),
		)
	}
	return joined, nil
}

// speechCall performs the provider I/O and reads the audio body. The endpoint
// answers with binary audio rather than JSON, which is why the raw value here is
// bytes.
func (d *driver) speechCall(
	ctx context.Context,
	request inference.GenerateRequest,
	params openai.AudioSpeechNewParams,
) ([]byte, error) {
	response, err := d.client.Audio.Speech.New(ctx, params)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("openai: reading speech audio: %w", err)
	}
	if len(data) == 0 {
		return nil, inference.NewError(
			inference.ProviderResponseUnusable,
			"",
			fmt.Errorf("openai: speech response carried no audio"),
		)
	}
	return data, nil
}

// speechDecode wraps the audio bytes in a content part.
func (d *driver) speechDecode(
	_ context.Context,
	request inference.GenerateRequest,
	raw []byte,
) (inference.GenerateResponse, error) {
	options := speechOptions(request.Extensions)
	format := speechFormat(options)

	source, err := media.NewAudioBytes(raw, speechMediaType(format))
	if err != nil {
		return inference.GenerateResponse{}, err
	}
	part := message.AudioPart{Source: source}
	if encoding, sampleRate, channels := speechEncoding(format); encoding != "" {
		part.Format = &media.AudioFormat{
			Encoding:     encoding,
			SampleRateHz: sampleRate,
			Channels:     channels,
		}
	}

	// Speech is billed per character, and this is the only producer the counter
	// has: reporting it keeps the accounting meaningful for a non-token endpoint.
	characters := int64(len([]rune(request.Input.Content.Text())))
	return inference.GenerateResponse{
		Message: message.Message{
			Role:    message.RoleAssistant,
			Content: message.Content{Parts: []message.Part{part}},
		},
		FinishReason: inference.FinishStop,
		Usage:        usage.Usage{InputCharacters: &characters},
		Metadata: inference.Metadata{
			Model: model.ModelID{Provider: ProviderID, Name: request.Model.ID.Name},
		},
	}, nil
}

// speechFormat is the effective audio format, defaulting to the provider's own.
func speechFormat(options *SpeechOptions) string {
	if options != nil && options.Format != "" {
		return options.Format
	}
	return "mp3"
}

// speechMediaType names the audio the part carries. It is derived from the
// encoding when there is one, so the source's media type and the declared format
// cannot disagree; a container format names itself instead.
func speechMediaType(format string) string {
	if encoding, _, _ := speechEncoding(format); encoding != "" {
		return encoding.MediaType()
	}
	return "audio/wav"
}

// speechEncoding maps the wire format onto the canonical encoding. A container
// format (wav) has no raw encoding, so it reports none rather than pretending to
// be PCM: the source's media type already names the container.
func speechEncoding(format string) (media.AudioEncoding, int, int) {
	switch format {
	case "mp3":
		return media.AudioEncodingMP3, 0, 0
	case "opus":
		return media.AudioEncodingOpus, 0, 0
	case "aac":
		return media.AudioEncodingAAC, 0, 0
	case "flac":
		return media.AudioEncodingFLAC, 0, 0
	case "pcm":
		// Documented by the provider: 24 kHz, 16-bit signed little-endian, mono.
		return media.AudioEncodingPCM16, 24000, 1
	default:
		return "", 0, 0
	}
}
