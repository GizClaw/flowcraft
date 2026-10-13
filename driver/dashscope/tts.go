package dashscope

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// ttsPath is the multimodal-generation surface the Qwen-TTS family answers
// speech synthesis on: the text models' endpoint, with input.text and
// input.voice in place of input.messages.
const ttsPath = generatePathMultimodal

// ttsWire is one compiled speech-synthesis call.
type ttsWire struct {
	model    string
	text     string
	voice    string
	language string
}

// ttsEnvelope is the Qwen-TTS answer: the generation envelope carrying the
// synthesized file in place of a content array. The platform reports refusals
// in band under an HTTP 200, like every other synchronous surface here.
type ttsEnvelope struct {
	StatusCode int    `json:"status_code"`
	Code       string `json:"code"`
	Message    string `json:"message"`
	RequestID  string `json:"request_id"`
	Output     struct {
		FinishReason string `json:"finish_reason"`
		Audio        struct {
			// Data carries base64 audio on the streamed surface's
			// intermediate frames; a unary answer sets URL instead.
			Data      string `json:"data"`
			URL       string `json:"url"`
			ID        string `json:"id"`
			ExpiresAt int64  `json:"expires_at"`
		} `json:"audio"`
	} `json:"output"`
	Usage struct {
		InputTokens  int64  `json:"input_tokens"`
		OutputTokens int64  `json:"output_tokens"`
		Characters   *int64 `json:"characters"`
	} `json:"usage"`
}

type ttsRaw struct {
	body ttsEnvelope
}

// compileTTS lowers one canonical speech request onto the Qwen-TTS surface.
// The prompt is the request's text in the order the caller wrote it, the voice
// is required, and the controls the surface does not carry — the encoding,
// sample-rate and channel asks, the speed dial, and a count above one — are
// refused rather than silently answered in a shape the caller did not ask for.
func compileTTS(declared ModelSpec, name string) inference.GenerateCompiler[ttsWire] {
	params := declared.TTS
	return func(
		_ context.Context,
		_ model.ModelRef,
		request inference.GenerateRequest,
		shape inference.GenerateExecutionShape,
	) (inference.Compiled[ttsWire], error) {
		ledger := inference.NewLedger(
			model.OperationGenerate,
			providerID,
			request.ActiveFieldsFor(shape),
		)
		if shape == inference.GenerateExecutionStream {
			ledger.Reject(
				inference.FieldGenerateExecutionStream,
				"speech synthesis is unary on this provider",
			)
		}
		wire := ttsWire{model: wireModel(name, declared)}
		prompt := turnParts(
			request,
			ledger,
			func(part message.Part, fields func(message.PartKind) inference.FieldID) {
				ledger.Reject(
					fields(part.Kind()),
					fmt.Sprintf("speech synthesis speaks text, not %s", part.Kind()),
				)
			},
			"speech synthesis keeps user context only",
		)
		wire.text = strings.Join(prompt, "\n")

		intent := request.Input.Content.Intent
		if text := intent.Text; text != nil {
			rejectTextControls(text, ledger,
				"speech models do not call tools",
				"speech synthesis has no sampling controls",
				"speech models have no thinking control",
			)
			ledger.Reject(
				inference.FieldGenerateIntentText,
				"speech models do not produce text",
			)
		}
		if intent.Image != nil {
			ledger.Reject(
				inference.FieldGenerateIntentImage,
				"speech models do not produce images",
			)
		}
		if intent.Video != nil {
			ledger.Reject(
				inference.FieldGenerateIntentVideo,
				"speech models do not produce video",
			)
		}
		switch audio := intent.Audio; audio {
		case nil:
			// The surface has no default voice: a request that names no
			// audio intent names no voice either, and the field of its own
			// is inactive exactly then.
			newRejectionTarget(request, shape).reject(
				ledger,
				"speech synthesis requires an audio intent carrying a voice",
				inference.FieldGenerateIntentAudio,
			)
		default:
			if audio.Voice.ID == "" {
				ledger.Reject(
					inference.FieldGenerateIntentAudioVoice,
					"speech synthesis requires a voice",
				)
			}
			wire.voice = audio.Voice.ID
			switch language := audio.Voice.Language; {
			case language == "":
			case !params.LanguageSelect:
				ledger.Reject(
					inference.FieldGenerateIntentAudioVoiceLanguage,
					"the declaration names no language control; set tts.language_select to serve input.language_type",
				)
			default:
				wire.language = language
			}
			compileTTSFormat(params, audio.Format, ledger)
			if audio.Speed != nil {
				ledger.Reject(
					inference.FieldGenerateIntentAudioSpeed,
					"the Qwen-TTS surface has no speed control; the pace belongs in the text",
				)
			}
			if audio.Count != nil && *audio.Count > 1 {
				ledger.Reject(
					inference.FieldGenerateIntentAudioCount,
					"speech synthesis produces a single audio stream",
				)
			}
		}
		rejectPromptLength(
			request, ledger,
			len([]rune(wire.text)), params.PromptLimit,
			fmt.Sprintf("%s caps the text at %d characters", name, params.PromptLimit),
		)
		ledger.RejectExtensions("speech synthesis", request.Extensions)

		report := ledger.Report()
		if ledger.Rejected() {
			return inference.Compiled[ttsWire]{Report: report}, ledger.Err()
		}
		return inference.Compiled[ttsWire]{Wire: wire, Report: report}, nil
	}
}

// compileTTSFormat checks the canonical format asks against the declaration.
// The surface carries no format control: the caller may ask for exactly what
// the model serves, and nothing in the wire body would show whether the ask
// was honored, so a mismatched value is refused on its own field rather than
// answered by a platform default the caller never chose.
func compileTTSFormat(params TTSParams, format media.AudioFormat, ledger *inference.Ledger) {
	if encoding := format.Encoding; encoding != "" && encoding != params.Encoding {
		switch params.Encoding {
		case "":
			ledger.Reject(
				inference.FieldGenerateIntentAudioFormatEncoding,
				"the declaration names no served encoding; set tts.encoding",
			)
		default:
			ledger.Reject(
				inference.FieldGenerateIntentAudioFormatEncoding,
				fmt.Sprintf(
					"the model serves %q and the surface has no encoding control; %q has no native token",
					params.Encoding, encoding,
				),
			)
		}
	}
	if rate := format.SampleRateHz; rate != 0 && rate != params.SampleRateHz {
		switch params.SampleRateHz {
		case 0:
			ledger.Reject(
				inference.FieldGenerateIntentAudioFormatSampleRate,
				"the declaration names no served sample rate; set tts.sample_rate_hz",
			)
		default:
			ledger.Reject(
				inference.FieldGenerateIntentAudioFormatSampleRate,
				fmt.Sprintf(
					"the model serves %d Hz and the surface has no sample-rate control",
					params.SampleRateHz,
				),
			)
		}
	}
	if channels := format.Channels; channels != 0 && channels != params.Channels {
		switch params.Channels {
		case 0:
			ledger.Reject(
				inference.FieldGenerateIntentAudioFormatChannels,
				"the declaration names no served channel count; set tts.channels",
			)
		default:
			ledger.Reject(
				inference.FieldGenerateIntentAudioFormatChannels,
				fmt.Sprintf(
					"the model serves %d channel(s) and the surface has no channel control",
					params.Channels,
				),
			)
		}
	}
}

// ttsRequest renders the wire body: the model, the text, the voice, and the
// language only when the caller named one and the declaration serves it.
func ttsRequest(wire ttsWire) map[string]any {
	input := map[string]any{
		"text":  wire.text,
		"voice": wire.voice,
	}
	if wire.language != "" {
		input["language_type"] = wire.language
	}
	return map[string]any{
		"model": wire.model,
		"input": input,
	}
}

// transportTTS posts one unary speech call. A non-200 answer is already
// classified by the request path; the in-band error fields of a 200 answer are
// classified here so a refusal never surfaces as an empty success.
func transportTTS(client *mediaClient) inference.Transport[ttsWire, ttsRaw] {
	return func(ctx context.Context, wire ttsWire) (ttsRaw, error) {
		var envelope ttsEnvelope
		if err := client.postJSON(ctx, ttsPath, false, ttsRequest(wire), &envelope); err != nil {
			inference.LogProviderCall(ctx, providerID, "speech synthesis", wire.model, err, "", "")
			return ttsRaw{}, err
		}
		if envelope.Code != "" {
			failure := errorBody{
				Code:      envelope.Code,
				Message:   envelope.Message,
				RequestID: envelope.RequestID,
			}.failure("speech synthesis", http.StatusOK)
			inference.LogProviderCall(
				ctx, providerID, "speech synthesis", wire.model, failure, envelope.RequestID, "")
			return ttsRaw{}, failure
		}
		if envelope.Output.Audio.URL == "" && envelope.Output.Audio.Data == "" {
			failure := fmt.Errorf("dashscope: speech synthesis returned no audio")
			inference.LogProviderCall(
				ctx, providerID, "speech synthesis", wire.model, failure, envelope.RequestID, "")
			return ttsRaw{}, failure
		}
		inference.LogProviderCall(
			ctx, providerID, "speech synthesis", wire.model, nil, envelope.RequestID, "")
		return ttsRaw{body: envelope}, nil
	}
}

// decodeTTS lowers one answer into a single audio part. A unary answer names
// the file by URL (24-hour retention); a surface that answers with inline
// audio instead is decoded into bytes, so both shapes reach the caller.
//
// The part states the declared payload encoding, which the canonical audio
// format requires and core's response validation checks against the request,
// and stays silent on the container: the platform wraps that payload in a WAV
// file, and the canonical alphabet has no container token, so the URL source
// names no media type rather than misnaming one. A caller that needs the
// container reads it off the payload's RIFF header, which the format's
// declared rate and channel count then have to match.
func decodeTTS(params TTSParams) inference.Decoder[ttsRaw, inference.GenerateResponse] {
	return func(
		_ context.Context,
		raw ttsRaw,
	) (inference.GenerateResponse, error) {
		audio := raw.body.Output.Audio
		var source media.AudioSource
		switch {
		case audio.URL != "":
			value, err := media.NewAudioURL(audio.URL, "")
			if err != nil {
				return inference.GenerateResponse{}, fmt.Errorf(
					"dashscope: speech synthesis audio URL: %w", err)
			}
			source = value
		default:
			payload, err := base64.StdEncoding.DecodeString(audio.Data)
			if err != nil {
				return inference.GenerateResponse{}, fmt.Errorf(
					"dashscope: speech synthesis audio payload: %w", err)
			}
			// Inline bytes have to name a media type, so the payload's own
			// encoding is the only honest one available.
			value, err := media.NewAudioBytes(payload, params.Encoding.MediaType())
			if err != nil {
				return inference.GenerateResponse{}, fmt.Errorf(
					"dashscope: speech synthesis audio payload: %w", err)
			}
			source = value
		}
		format := media.AudioFormat{
			Encoding:     params.Encoding,
			SampleRateHz: params.SampleRateHz,
			Channels:     params.Channels,
		}
		return inference.GenerateResponse{
			Message: message.Message{
				Role: message.RoleAssistant,
				Content: message.Content{Parts: []message.Part{
					message.AudioPart{Source: source, Format: &format},
				}},
			},
			FinishReason: inference.FinishCompleted,
			Usage:        inference.Usage{InputCharacters: raw.body.Usage.Characters},
			Metadata: inference.Metadata{
				RequestID:  raw.body.RequestID,
				ResponseID: audio.ID,
			},
		}, nil
	}
}

// openTTS binds the speech compiler to the multimodal-generation surface. The
// surface's streamed form delivers the same audio in SSE frames, which this
// driver does not serve yet, so the operations carry no stream driver and a
// streaming request is refused at compile time.
func openTTS(
	cls *clients,
	declared ModelSpec,
	id model.ModelID,
) (inference.GenerateOperations, error) {
	unary, err := inference.BindGenerate(
		compileTTS(declared, id.Name),
		transportTTS(cls.media),
		decodeTTS(declared.TTS),
	)
	if err != nil {
		return inference.GenerateOperations{}, err
	}
	return inference.GenerateOperations{Unary: unary}, nil
}
