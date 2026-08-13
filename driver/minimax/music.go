package minimax

import (
	"context"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// Music generation runs on the music_generation endpoint: the request's
// text is the style prompt; lyrics, the instrumental switch, and the AIGC
// watermark ride the MusicOptions extension. The canonical audio intent
// serves voice-free synthesis here: setting a voice rejects, as does
// speed (music has no tempo knob). Output is stereo, so only a 2-channel
// format request is truthful — anything else rejects. Streaming delivers
// the same SSE hex shape as speech synthesis, so both drivers share the
// hex audio stream scanner.
//
// Scope: the text-to-music models (music-3.0/2.6 and their -free tiers).
// music-cover needs a reference audio plus the two-step cover_feature_id
// preprocessing flow, which has no canonical surface — it stays out of the
// catalog rather than being approximated.

type musicWire struct {
	model        string
	prompt       string
	stream       bool
	lyrics       string
	instrumental *bool
	optimizer    bool
	watermark    *bool
	format       string // mp3 | pcm
	sample       int
}

type musicRaw struct {
	data      []byte
	format    media.AudioFormat
	requestID string
}

var musicSampleRates = map[int]bool{
	16000: true, 24000: true, 32000: true, 44100: true,
}

func compileMusic(
	endpoint string,
) inference.GenerateCompiler[musicWire] {
	return func(
		_ context.Context,
		model inference.ModelRef,
		request inference.GenerateRequest,
		shape inference.GenerateExecutionShape,
	) (inference.Compiled[musicWire], error) {
		ledger := newLedger(
			inference.OperationGenerate,
			request.ActiveFieldsFor(shape),
		)
		wire := musicWire{
			model:  endpoint,
			stream: shape == inference.GenerateExecutionStream,
		}

		var prompt []string
		collect := func(parts []message.Part, fields map[message.PartKind]inference.FieldID) {
			for _, part := range parts {
				if value, ok := part.(message.TextPart); ok {
					prompt = append(prompt, value.Text)
					continue
				}
				ledger.reject(
					fields[part.Kind()],
					fmt.Sprintf("music generation takes a text style prompt, not %s", part.Kind()),
				)
			}
		}
		for _, turn := range request.Context {
			if turn.Role != message.RoleUser {
				ledger.reject(
					inference.FieldGenerateContextRole,
					"music generation keeps user context only",
				)
				continue
			}
			collect(turn.Content.Parts, contextPartFields)
		}
		collect(request.Input.Content.Parts, inputPartFields)
		wire.prompt = strings.Join(prompt, "\n")

		intent := request.Input.Content.Intent
		if text := intent.Text; text != nil {
			rejectTextControls(text, ledger,
				"music models do not call tools",
				"music generation has no sampling controls",
				"music models have no thinking control",
			)
			ledger.reject(
				inference.FieldGenerateIntentText,
				"music models do not produce text",
			)
		}
		if intent.Image != nil {
			ledger.reject(
				inference.FieldGenerateIntentImage,
				"music models do not produce images",
			)
		}
		if intent.Video != nil {
			ledger.reject(
				inference.FieldGenerateIntentVideo,
				"music models do not produce video",
			)
		}
		if audio := intent.Audio; audio != nil {
			if audio.Voice.ID != "" {
				ledger.reject(
					inference.FieldGenerateIntentAudioVoiceID,
					"music generation has no voice; omit the voice and put lyrics in MusicOptions",
				)
			}
			if audio.Voice.Language != "" {
				ledger.reject(
					inference.FieldGenerateIntentAudioVoiceLanguage,
					"music generation has no voice language",
				)
			}
			compileMusicFormat(&wire, audio.Format, ledger)
			if audio.Speed != nil {
				ledger.reject(
					inference.FieldGenerateIntentAudioSpeed,
					"music generation has no speed control",
				)
			}
			if audio.Count != nil && *audio.Count > 1 {
				ledger.reject(
					inference.FieldGenerateIntentAudioCount,
					"music generation produces a single track",
				)
			}
		}

		options, other := operationExtensions[MusicOptions](request.Extensions)
		rejectOtherExtensions("music generation", other, ledger)
		wire.lyrics = options.Lyrics
		wire.instrumental = options.Instrumental
		wire.optimizer = options.LyricsOptimizer
		wire.watermark = options.Watermark
		if wire.watermark != nil && wire.stream {
			ledger.reject(
				inference.ExtensionField("watermark").Qualify(options),
				"the AIGC watermark applies to unary requests only",
			)
		}

		report := ledger.report()
		if len(ledger.order) > 0 {
			return inference.Compiled[musicWire]{Report: report}, ledger.err()
		}
		return inference.Compiled[musicWire]{Wire: wire, Report: report}, nil
	}
}

func compileMusicFormat(wire *musicWire, format media.AudioFormat, ledger *ledger) {
	switch format.Encoding {
	case "":
		// Unset: the endpoint defaults to mp3.
	case media.AudioEncodingMP3:
		wire.format = "mp3"
	case media.AudioEncodingPCM16:
		wire.format = "pcm"
	case media.AudioEncodingFLAC, media.AudioEncodingOpus,
		media.AudioEncodingPCM24, media.AudioEncodingFloat32,
		media.AudioEncodingAAC:
		ledger.reject(
			inference.FieldGenerateIntentAudioFormatEncoding,
			fmt.Sprintf("minimax music encodes mp3 or 16-bit pcm, not %s", format.Encoding),
		)
	default:
		ledger.reject(
			inference.FieldGenerateIntentAudioFormatEncoding,
			fmt.Sprintf("minimax music cannot encode %s", format.Encoding),
		)
	}
	if format.SampleRateHz != 0 {
		if !musicSampleRates[format.SampleRateHz] {
			ledger.reject(
				inference.FieldGenerateIntentAudioFormatSampleRate,
				fmt.Sprintf("minimax music sample rates are 16000/24000/32000/44100, not %d", format.SampleRateHz),
			)
		} else {
			wire.sample = format.SampleRateHz
		}
	}
	if format.Channels != 0 && format.Channels != 2 {
		ledger.reject(
			inference.FieldGenerateIntentAudioFormatChannels,
			fmt.Sprintf("minimax music output is stereo, not %d channels", format.Channels),
		)
	}
}

// musicCanonicalFormat echoes the negotiated format for the response part;
// it is derived from the request, never from provider payloads.
func musicCanonicalFormat(wire musicWire) media.AudioFormat {
	format := media.AudioFormat{Encoding: media.AudioEncodingMP3}
	if wire.format == "pcm" {
		format.Encoding = media.AudioEncodingPCM16
	}
	format.SampleRateHz = wire.sample
	if format.Encoding == media.AudioEncodingPCM16 {
		if format.SampleRateHz == 0 {
			format.SampleRateHz = 44100
		}
		format.Channels = 2 // the model's fixed output layout
	}
	return format
}

// musicRequest renders the music_generation payload.
func musicRequest(wire musicWire) map[string]any {
	request := map[string]any{
		"model":         wire.model,
		"prompt":        wire.prompt,
		"stream":        wire.stream,
		"output_format": "hex",
	}
	if wire.lyrics != "" {
		request["lyrics"] = wire.lyrics
	}
	if wire.instrumental != nil {
		request["is_instrumental"] = *wire.instrumental
	}
	if wire.optimizer {
		request["lyrics_optimizer"] = true
	}
	if wire.watermark != nil && !wire.stream {
		request["aigc_watermark"] = *wire.watermark
	}
	if wire.format != "" || wire.sample != 0 {
		audio := map[string]any{}
		if wire.format != "" {
			audio["format"] = wire.format
		}
		if wire.sample != 0 {
			audio["sample_rate"] = wire.sample
		}
		request["audio_setting"] = audio
	}
	return request
}

// musicResponse is the unary music_generation envelope.
type musicResponse struct {
	Data struct {
		Audio string `json:"audio"`
	} `json:"data"`
	// TraceID is the server-assigned request trace id; the docs' example
	// places it at the envelope root alongside data and base_resp.
	TraceID  string   `json:"trace_id"`
	BaseResp baseResp `json:"base_resp"`
}

func transportMusic(
	client *mediaClient,
) inference.Transport[musicWire, musicRaw] {
	return func(ctx context.Context, wire musicWire) (musicRaw, error) {
		var resp musicResponse
		if err := client.postJSON(ctx, "/v1/music_generation", musicRequest(wire), &resp); err != nil {
			return musicRaw{}, err
		}
		if err := resp.BaseResp.err("music generation"); err != nil {
			return musicRaw{}, err
		}
		data, err := hexDecode(resp.Data.Audio)
		if err != nil {
			return musicRaw{}, err
		}
		return musicRaw{
			data:      data,
			format:    musicCanonicalFormat(wire),
			requestID: resp.TraceID,
		}, nil
	}
}

func decodeMusic(
	_ context.Context,
	raw musicRaw,
) (inference.GenerateResponse, error) {
	source, err := media.NewAudioBytes(raw.data, raw.format.Encoding.MediaType())
	if err != nil {
		return inference.GenerateResponse{}, fmt.Errorf("minimax: music payload: %w", err)
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

func transportMusicStream(
	client *mediaClient,
) inference.Transport[musicWire, inference.ProviderStream[hexAudioStreamRaw]] {
	return func(
		ctx context.Context,
		wire musicWire,
	) (inference.ProviderStream[hexAudioStreamRaw], error) {
		body, err := client.postSSE(ctx, "/v1/music_generation", musicRequest(wire))
		if err != nil {
			return nil, err
		}
		return newHexAudioStream(body, musicCanonicalFormat(wire)), nil
	}
}

func openMusic(
	cls *clients,
	_ catalogEntry,
	id inference.ModelID,
) (inference.GenerateOperations, error) {
	return inference.BindGenerateOperations(
		compileMusic(id.Name),
		transportMusic(cls.media),
		decodeMusic,
		transportMusicStream(cls.media),
		decodeHexAudioStream,
	)
}
