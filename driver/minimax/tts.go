package minimax

import (
	"context"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// Speech synthesis runs on the t2a_v2 endpoint: one JSON request, audio
// back as hex — unary in one payload, streaming as SSE chunks. The
// request's text is spoken as-is. MiniMax speeds range [0.5, 2], so
// canonical speeds outside that band are rejected instead of clamped; pcm
// output is 16-bit, so only PCM16 maps truthfully among the raw encodings.

type ttsWire struct {
	model    string
	text     string
	stream   bool
	voice    string
	language string
	speed    *float64
	format   string // mp3 | pcm | flac | opus
	sample   int
	channels int
}

type ttsRaw struct {
	data      []byte
	format    media.AudioFormat
	requestID string
}

var ttsSampleRates = map[int]bool{
	8000: true, 16000: true, 22050: true, 24000: true, 32000: true, 44100: true,
}

func compileTTS(
	endpoint string,
) inference.GenerateCompiler[ttsWire] {
	return func(
		_ context.Context,
		model inference.ModelRef,
		request inference.GenerateRequest,
		shape inference.GenerateExecutionShape,
	) (inference.Compiled[ttsWire], error) {
		ledger := newLedger(
			inference.OperationGenerate,
			request.ActiveFieldsFor(shape),
		)
		wire := ttsWire{
			model:  endpoint,
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
				"speech models have no thinking control",
			)
			ledger.reject(
				inference.FieldGenerateIntentText,
				"speech models do not produce text",
			)
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
		if audio := intent.Audio; audio != nil {
			if audio.Voice.ID == "" {
				ledger.reject(
					inference.FieldGenerateIntentAudioVoice,
					"speech synthesis requires a voice",
				)
			}
			wire.voice = audio.Voice.ID
			wire.language = audio.Voice.Language
			compileTTSFormat(&wire, audio.Format, ledger)
			if audio.Speed != nil {
				speed := *audio.Speed
				if speed < 0.5 || speed > 2 {
					ledger.reject(
						inference.FieldGenerateIntentAudioSpeed,
						fmt.Sprintf("minimax speech speed ranges 0.5 to 2, not %g", speed),
					)
				} else {
					wire.speed = audio.Speed
				}
			}
			if audio.Count != nil && *audio.Count > 1 {
				ledger.reject(
					inference.FieldGenerateIntentAudioCount,
					"speech synthesis produces a single audio stream",
				)
			}
		}
		rejectOtherExtensions("speech synthesis", request.Extensions, ledger)

		report := ledger.report()
		if len(ledger.order) > 0 {
			return inference.Compiled[ttsWire]{Report: report}, ledger.err()
		}
		return inference.Compiled[ttsWire]{Wire: wire, Report: report}, nil
	}
}

func compileTTSFormat(wire *ttsWire, format media.AudioFormat, ledger *ledger) {
	switch format.Encoding {
	case "":
		// Unset: the endpoint defaults to mp3.
	case media.AudioEncodingMP3:
		wire.format = "mp3"
	case media.AudioEncodingFLAC:
		wire.format = "flac"
	case media.AudioEncodingOpus:
		wire.format = "opus"
	case media.AudioEncodingPCM16:
		wire.format = "pcm"
	case media.AudioEncodingPCM24, media.AudioEncodingFloat32, media.AudioEncodingAAC:
		ledger.reject(
			inference.FieldGenerateIntentAudioFormatEncoding,
			fmt.Sprintf("minimax speech has no %s encoding; pcm output is 16-bit", format.Encoding),
		)
	default:
		ledger.reject(
			inference.FieldGenerateIntentAudioFormatEncoding,
			fmt.Sprintf("minimax speech cannot encode %s", format.Encoding),
		)
	}
	if format.SampleRateHz != 0 {
		if !ttsSampleRates[format.SampleRateHz] {
			ledger.reject(
				inference.FieldGenerateIntentAudioFormatSampleRate,
				fmt.Sprintf("minimax speech sample rates are 8000/16000/22050/24000/32000/44100, not %d", format.SampleRateHz),
			)
		} else {
			wire.sample = format.SampleRateHz
		}
	}
	if format.Channels != 0 {
		if format.Channels < 1 || format.Channels > 2 {
			ledger.reject(
				inference.FieldGenerateIntentAudioFormatChannels,
				fmt.Sprintf("minimax speech channels are 1 or 2, not %d", format.Channels),
			)
		} else {
			wire.channels = format.Channels
		}
	}
}

// ttsCanonicalFormat echoes the negotiated format for the response part;
// it is derived from the request, never from provider payloads.
func ttsCanonicalFormat(wire ttsWire) media.AudioFormat {
	format := media.AudioFormat{Encoding: media.AudioEncodingMP3}
	switch wire.format {
	case "flac":
		format.Encoding = media.AudioEncodingFLAC
	case "opus":
		format.Encoding = media.AudioEncodingOpus
	case "pcm":
		format.Encoding = media.AudioEncodingPCM16
	}
	format.SampleRateHz = wire.sample
	format.Channels = wire.channels
	if format.Encoding == media.AudioEncodingPCM16 {
		if format.SampleRateHz == 0 {
			format.SampleRateHz = 32000
		}
		if format.Channels == 0 {
			format.Channels = 1
		}
	}
	return format
}

// ttsRequest renders the t2a_v2 payload.
func ttsRequest(wire ttsWire) map[string]any {
	voice := map[string]any{"voice_id": wire.voice}
	if wire.speed != nil {
		voice["speed"] = *wire.speed
	}
	audio := map[string]any{}
	if wire.format != "" {
		audio["format"] = wire.format
	}
	if wire.sample != 0 {
		audio["sample_rate"] = wire.sample
	}
	if wire.channels != 0 {
		audio["channel"] = wire.channels
	}
	request := map[string]any{
		"model":          wire.model,
		"text":           wire.text,
		"stream":         wire.stream,
		"output_format":  "hex",
		"voice_setting":  voice,
		"audio_setting":  audio,
		"language_boost": "auto",
	}
	if wire.language != "" {
		request["language_boost"] = wire.language
	}
	return request
}

// ttsResponse is the unary t2a_v2 envelope.
type ttsResponse struct {
	Data struct {
		Audio string `json:"audio"`
	} `json:"data"`
	// TraceID is the server-assigned session id the docs describe as
	// "used for troubleshooting and support".
	TraceID  string   `json:"trace_id"`
	BaseResp baseResp `json:"base_resp"`
}

func transportTTS(
	client *mediaClient,
) inference.Transport[ttsWire, ttsRaw] {
	return func(ctx context.Context, wire ttsWire) (ttsRaw, error) {
		var resp ttsResponse
		if err := client.postJSON(ctx, "/v1/t2a_v2", ttsRequest(wire), &resp); err != nil {
			return ttsRaw{}, err
		}
		if err := resp.BaseResp.err("speech synthesis"); err != nil {
			return ttsRaw{}, err
		}
		data, err := hexDecode(resp.Data.Audio)
		if err != nil {
			return ttsRaw{}, err
		}
		return ttsRaw{
			data:      data,
			format:    ttsCanonicalFormat(wire),
			requestID: resp.TraceID,
		}, nil
	}
}

func decodeTTS(
	_ context.Context,
	raw ttsRaw,
) (inference.GenerateResponse, error) {
	source, err := media.NewAudioBytes(raw.data, raw.format.Encoding.MediaType())
	if err != nil {
		return inference.GenerateResponse{}, fmt.Errorf("minimax: audio payload: %w", err)
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
		Metadata:     inference.Metadata{RequestID: raw.requestID},
	}, nil
}

func transportTTSStream(
	client *mediaClient,
) inference.Transport[ttsWire, inference.ProviderStream[hexAudioStreamRaw]] {
	return func(
		ctx context.Context,
		wire ttsWire,
	) (inference.ProviderStream[hexAudioStreamRaw], error) {
		body, err := client.postSSE(ctx, "/v1/t2a_v2", ttsRequest(wire))
		if err != nil {
			return nil, err
		}
		return newHexAudioStream(body, ttsCanonicalFormat(wire)), nil
	}
}

func openTTS(
	cls *clients,
	_ catalogEntry,
	id inference.ModelID,
) (inference.GenerateOperations, error) {
	return inference.BindGenerateOperations(
		compileTTS(id.Name),
		transportTTS(cls.media),
		decodeTTS,
		transportTTSStream(cls.media),
		decodeHexAudioStream,
	)
}
