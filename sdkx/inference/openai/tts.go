package openai

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/message/media"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
)

// Speech synthesis runs on the audio speech endpoint. The request's text is
// spoken as-is. The endpoint has no language, sample-rate, or channel
// controls, and the provider's speed range is [0.25, 4.0], so requests
// outside those bounds are rejected instead of clamped or silently dropped.

type ttsWire struct {
	model  string
	text   string
	voice  string
	format string // mp3 | opus | aac | flac | pcm
	speed  *float64
	stream bool
	// canonicalFormat echoes the negotiated format for the response part;
	// it is derived from the request, never from provider payloads.
	canonicalEncoding  string
	canonicalMediaType string
	canonicalChannels  int
}

type ttsRaw struct {
	data   []byte
	format media.AudioFormat
}

// ttsStreamRaw carries the negotiated format with every delta: the format is
// fixed at compile time, flows compile → transport → raw, and never passes
// through the stateless decoder's construction site.
type ttsStreamRaw struct {
	data   []byte
	format *media.AudioFormat
	last   bool
}

func compileTTS(
	model string,
) inference.GenerateCompiler[ttsWire] {
	return func(
		_ context.Context,
		_ inference.ModelRef,
		request inference.GenerateRequest,
		shape inference.GenerateExecutionShape,
	) (inference.Compiled[ttsWire], error) {
		ledger := newLedger(
			inference.OperationGenerate,
			request.ActiveFieldsFor(shape),
		)
		wire := ttsWire{
			model:  model,
			stream: shape == inference.GenerateExecutionStream,
		}

		var text []string
		collect := func(parts []message.Part, fields map[message.PartKind]inference.FieldID) {
			for _, part := range parts {
				if value, ok := part.(message.TextPart); ok {
					text = append(text, value.Text)
					continue
				}
				ledger.reject(
					fields[part.Kind()],
					fmt.Sprintf("speech synthesis speaks text, not %s", part.Kind()),
				)
			}
		}
		for _, turn := range request.Context {
			if turn.Role != message.RoleUser {
				ledger.reject(
					inference.FieldGenerateContextRole,
					"speech synthesis keeps user context only",
				)
				continue
			}
			collect(turn.Content.Parts, contextPartFields)
		}
		collect(request.Input.Content.Parts, inputPartFields)
		wire.text = strings.Join(text, "\n")

		intent := request.Input.Content.Intent
		if text := intent.Text; text != nil {
			rejectTextControls(text, ledger,
				"speech models do not call tools",
				"speech synthesis has no sampling controls",
				"speech models have no reasoning control",
			)
			ledger.reject(
				inference.FieldGenerateIntentText,
				"speech models do not produce text",
			)
		}
		if audio := intent.Audio; audio != nil {
			if audio.Voice.ID == "" {
				ledger.reject(
					inference.FieldGenerateIntentAudioVoice,
					"speech synthesis requires a voice",
				)
			}
			wire.voice = audio.Voice.ID
			if audio.Voice.Language != "" {
				ledger.reject(
					inference.FieldGenerateIntentAudioVoiceLanguage,
					"the speech API has no language parameter",
				)
			}
			compileTTSFormat(&wire, audio.Format, ledger)
			if audio.Speed != nil {
				speed := *audio.Speed
				if speed < 0.25 || speed > 4.0 {
					ledger.reject(
						inference.FieldGenerateIntentAudioSpeed,
						fmt.Sprintf("provider supports speed between 0.25 and 4, not %g", speed),
					)
				} else {
					wire.speed = &speed
				}
			}
			if audio.Count != nil && *audio.Count > 1 {
				ledger.reject(
					inference.FieldGenerateIntentAudioCount,
					"speech synthesis produces a single audio stream",
				)
			}
		}
		if intent.Image != nil {
			ledger.reject(
				inference.FieldGenerateIntentImage,
				"speech models do not produce images",
			)
		}
		if intent.Video != nil {
			ledger.reject(
				inference.FieldGenerateIntentVideo,
				"speech models do not produce video",
			)
		}
		for _, field := range request.Extensions.ActiveFields() {
			ledger.reject(field, "openai speech synthesis supports no extensions")
		}

		report := ledger.report()
		if len(ledger.order) > 0 {
			return inference.Compiled[ttsWire]{Report: report}, ledger.err()
		}
		return inference.Compiled[ttsWire]{Wire: wire, Report: report}, nil
	}
}

// compileTTSFormat maps the canonical audio format onto endpoint tokens.
// Raw PCM is 24kHz mono on this API, with no rate or channel controls, so
// only the encoding round-trips.
func compileTTSFormat(
	wire *ttsWire,
	format media.AudioFormat,
	ledger *ledger,
) {
	switch format.Encoding {
	case "":
		// Unset: the provider default (mp3) applies; nothing is negotiated.
	case media.AudioEncodingPCM16:
		wire.format = "pcm"
	case media.AudioEncodingMP3:
		wire.format = "mp3"
	case media.AudioEncodingOpus:
		wire.format = "opus"
	case media.AudioEncodingAAC:
		wire.format = "aac"
	case media.AudioEncodingFLAC:
		wire.format = "flac"
	default:
		ledger.reject(
			inference.FieldGenerateIntentAudioFormatEncoding,
			fmt.Sprintf("audio encoding %q has no native token", format.Encoding),
		)
		return
	}
	if format.SampleRateHz != 0 {
		ledger.reject(
			inference.FieldGenerateIntentAudioFormatSampleRate,
			"the speech API has no sample-rate control",
		)
		return
	}
	if format.Channels > 1 {
		ledger.reject(
			inference.FieldGenerateIntentAudioFormatChannels,
			"speech synthesis is mono",
		)
		return
	}
	if format.Encoding == "" {
		return
	}
	wire.canonicalEncoding = string(format.Encoding)
	wire.canonicalMediaType = format.Encoding.MediaType()
	wire.canonicalChannels = 1
}

// ttsParams renders the endpoint request body.
func ttsParams(wire ttsWire) openai.AudioSpeechNewParams {
	params := openai.AudioSpeechNewParams{
		Model: wire.model,
		Input: wire.text,
		Voice: openai.AudioSpeechNewParamsVoiceUnion{
			OfAudioSpeechNewsVoiceString2: openai.String(wire.voice),
		},
	}
	if wire.format != "" {
		params.ResponseFormat = openai.AudioSpeechNewParamsResponseFormat(wire.format)
	}
	if wire.speed != nil {
		params.Speed = param.NewOpt(*wire.speed)
	}
	return params
}

// ttsCanonicalFormat rebuilds the negotiated canonical format from the wire.
// An unset encoding means the provider default (mp3) is what the payload
// will actually carry.
func ttsCanonicalFormat(wire ttsWire) media.AudioFormat {
	encoding := wire.canonicalEncoding
	if encoding == "" {
		encoding = string(media.AudioEncodingMP3)
	}
	format := media.AudioFormat{
		Encoding: media.AudioEncoding(encoding),
		Channels: wire.canonicalChannels,
	}
	if format.Encoding == media.AudioEncodingPCM16 && format.Channels == 0 {
		format.Channels = 1
	}
	return format
}

// ---------------------------------------------------------------------------
// Unary: drain the audio body into one payload.
// ---------------------------------------------------------------------------

func transportTTS(
	client openai.Client,
) inference.Transport[ttsWire, ttsRaw] {
	return func(ctx context.Context, wire ttsWire) (ttsRaw, error) {
		body, err := client.Audio.Speech.New(ctx, ttsParams(wire))
		if err != nil {
			return ttsRaw{}, classifyError(err)
		}
		defer func() { _ = body.Body.Close() }()
		data, err := io.ReadAll(body.Body)
		if err != nil {
			return ttsRaw{}, classifyError(err)
		}
		if len(data) == 0 {
			return ttsRaw{}, fmt.Errorf("openai: synthesis produced no audio")
		}
		return ttsRaw{data: data, format: ttsCanonicalFormat(wire)}, nil
	}
}

func decodeTTS(
	_ context.Context,
	raw ttsRaw,
) (inference.GenerateResponse, error) {
	source, err := media.NewAudioBytes(raw.data, raw.format.Encoding.MediaType())
	if err != nil {
		return inference.GenerateResponse{}, fmt.Errorf("openai: audio payload: %w", err)
	}
	format := raw.format
	return inference.GenerateResponse{
		Message: message.Message{
			Role: message.RoleAssistant,
			Content: message.Content{Parts: []message.Part{
				message.AudioPart{Source: source, Format: &format},
			}},
		},
		FinishReason: inference.FinishCompleted,
	}, nil
}

// ---------------------------------------------------------------------------
// Stream: fixed-size body reads become audio deltas. The endpoint streams
// raw bytes with no sentinel, so body end marks the finish event.
// ---------------------------------------------------------------------------

const ttsStreamChunkSize = 16 * 1024

type ttsStream struct {
	body   io.ReadCloser
	format media.AudioFormat

	emitFinish bool // body drained; finish event pending
	done       bool // finish delivered; next Next returns EOF
}

func transportTTSStream(
	client openai.Client,
) inference.Transport[ttsWire, inference.ProviderStream[ttsStreamRaw]] {
	return func(
		ctx context.Context,
		wire ttsWire,
	) (inference.ProviderStream[ttsStreamRaw], error) {
		body, err := client.Audio.Speech.New(ctx, ttsParams(wire))
		if err != nil {
			return nil, classifyError(err)
		}
		return &ttsStream{
			body:   body.Body,
			format: ttsCanonicalFormat(wire),
		}, nil
	}
}

func (s *ttsStream) Next(ctx context.Context) (ttsStreamRaw, error) {
	if err := ctx.Err(); err != nil {
		return ttsStreamRaw{}, err
	}
	for {
		if s.emitFinish {
			s.emitFinish = false
			s.done = true
			return ttsStreamRaw{last: true}, nil
		}
		if s.done {
			return ttsStreamRaw{}, io.EOF
		}
		buffer := make([]byte, ttsStreamChunkSize)
		n, err := s.body.Read(buffer)
		if n > 0 {
			format := s.format
			return ttsStreamRaw{data: buffer[:n], format: &format}, nil
		}
		if err == io.EOF {
			s.emitFinish = true
			continue
		}
		if err != nil {
			return ttsStreamRaw{}, classifyError(err)
		}
		// (0, nil): read again.
	}
}

func (s *ttsStream) Close() error {
	return classifyError(s.body.Close())
}

// decodeTTSStream turns body reads into audio deltas. It is pure: the
// negotiated format arrives on every raw delta.
func decodeTTSStream(
	_ context.Context,
	raw ttsStreamRaw,
) (inference.GenerateStreamEvent, error) {
	if raw.last {
		return inference.GenerateStreamEvent{
			FinishReason: inference.FinishCompleted,
		}, nil
	}
	if len(raw.data) == 0 || raw.format == nil {
		return inference.GenerateStreamEvent{}, fmt.Errorf(
			"openai: synthesis chunk carried no audio",
		)
	}
	format := *raw.format
	return inference.GenerateStreamEvent{
		Delta: inference.AudioPartDelta{Data: raw.data, Format: &format},
	}, nil
}

func openTTS(
	cls *clients,
	id inference.ModelID,
	_ string,
) (inference.GenerateOperations, error) {
	return KernelTTS(cls.api, id.Name)
}
