package openai

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
)

// Speech synthesis runs on the audio speech endpoint. The request's text is
// spoken as-is. The endpoint has no language, sample-rate, or channel
// controls, and the provider's speed range is [0.25, 4.0], so requests
// outside those bounds are rejected instead of clamped or silently dropped.

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
	modelName string,
) inference.GenerateCompiler[openai.AudioSpeechNewParams] {
	return func(
		_ context.Context,
		_ model.ModelRef,
		request inference.GenerateRequest,
		shape inference.GenerateExecutionShape,
	) (inference.Compiled[openai.AudioSpeechNewParams], error) {
		ledger := inference.NewLedger(
			model.OperationGenerate,
			providerID,
			request.ActiveFieldsFor(shape),
		)
		params := openai.AudioSpeechNewParams{Model: modelName}

		var text []string
		collect := func(parts []message.Part, fields func(message.PartKind) inference.FieldID) {
			for _, part := range parts {
				if value, ok := part.(message.TextPart); ok {
					text = append(text, value.Text)
					continue
				}
				ledger.Reject(
					fields(part.Kind()),
					fmt.Sprintf("speech synthesis speaks text, not %s", part.Kind()),
				)
			}
		}
		for _, turn := range request.Context {
			if turn.Role != message.RoleUser {
				ledger.Reject(
					inference.FieldGenerateContextRole,
					"speech synthesis keeps user context only",
				)
				continue
			}
			collect(turn.Content.Parts, contextPartField)
		}
		collect(request.Input.Content.Parts, inputPartField)
		params.Input = strings.Join(text, "\n")

		intent := request.Input.Content.Intent
		if text := intent.Text; text != nil {
			rejectTextControls(text, ledger,
				"speech models do not call tools",
				"speech synthesis has no sampling controls",
				"speech models have no reasoning control",
			)
			ledger.Reject(
				inference.FieldGenerateIntentText,
				"speech models do not produce text",
			)
		}
		if audio := intent.Audio; audio != nil {
			if audio.Voice.ID == "" {
				ledger.Reject(
					inference.FieldGenerateIntentAudioVoice,
					"speech synthesis requires a voice",
				)
			}
			params.Voice = openai.AudioSpeechNewParamsVoiceUnion{
				OfAudioSpeechNewsVoiceString2: openai.String(audio.Voice.ID),
			}
			if audio.Voice.Language != "" {
				ledger.Reject(
					inference.FieldGenerateIntentAudioVoiceLanguage,
					"the speech API has no language parameter",
				)
			}
			compileTTSFormat(&params, audio.Format, ledger)
			if audio.Speed != nil {
				speed := *audio.Speed
				if speed < 0.25 || speed > 4.0 {
					ledger.Reject(
						inference.FieldGenerateIntentAudioSpeed,
						fmt.Sprintf("provider supports speed between 0.25 and 4, not %g", speed),
					)
				} else {
					params.Speed = param.NewOpt(speed)
				}
			}
			if audio.Count != nil && *audio.Count > 1 {
				ledger.Reject(
					inference.FieldGenerateIntentAudioCount,
					"speech synthesis produces a single audio stream",
				)
			}
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
		options, other := inference.ExtensionFor[TTSOptions](request.Extensions)
		ledger.RejectExtensions("speech synthesis", other)
		if instructions := options.Instructions; instructions != nil {
			params.Instructions = param.NewOpt(*instructions)
		}

		report := ledger.Report()
		if ledger.Rejected() {
			return inference.Compiled[openai.AudioSpeechNewParams]{Report: report}, ledger.Err()
		}
		return inference.Compiled[openai.AudioSpeechNewParams]{
			Wire:   params,
			Report: report,
		}, nil
	}
}

// compileTTSFormat maps the canonical audio format onto endpoint tokens.
// Raw PCM is 24kHz mono on this API, with no rate or channel controls, so only
// the encoding reaches the request.
func compileTTSFormat(
	params *openai.AudioSpeechNewParams,
	format media.AudioFormat,
	ledger *inference.Ledger,
) {
	switch format.Encoding {
	case "":
		// Unset: the provider default (mp3) applies; nothing is negotiated.
	case media.AudioEncodingPCM16:
		params.ResponseFormat = openai.AudioSpeechNewParamsResponseFormatPCM
	case media.AudioEncodingMP3:
		params.ResponseFormat = openai.AudioSpeechNewParamsResponseFormatMP3
	case media.AudioEncodingOpus:
		params.ResponseFormat = openai.AudioSpeechNewParamsResponseFormatOpus
	case media.AudioEncodingAAC:
		params.ResponseFormat = openai.AudioSpeechNewParamsResponseFormatAAC
	case media.AudioEncodingFLAC:
		params.ResponseFormat = openai.AudioSpeechNewParamsResponseFormatFLAC
	default:
		ledger.Reject(
			inference.FieldGenerateIntentAudioFormatEncoding,
			fmt.Sprintf("audio encoding %q has no native token", format.Encoding),
		)
		return
	}
	if format.SampleRateHz != 0 {
		ledger.Reject(
			inference.FieldGenerateIntentAudioFormatSampleRate,
			"the speech API has no sample-rate control",
		)
		return
	}
	if format.Channels > 1 {
		ledger.Reject(
			inference.FieldGenerateIntentAudioFormatChannels,
			"speech synthesis is mono",
		)
	}
}

// ttsCanonicalFormat rebuilds the negotiated canonical format from the
// compiled request. An unset response format means the provider default (mp3)
// is what the payload will actually carry; the endpoint is mono, so an
// negotiated encoding reports one channel. The endpoint spells the canonical
// pcm16 stream "pcm", so the two vocabularies meet here and nowhere else.
func ttsCanonicalFormat(params openai.AudioSpeechNewParams) media.AudioFormat {
	switch params.ResponseFormat {
	case "":
		// Provider default: mp3. The endpoint negotiated nothing, so no
		// channel count is claimed.
		return media.AudioFormat{Encoding: media.AudioEncodingMP3}
	case openai.AudioSpeechNewParamsResponseFormatPCM:
		return media.AudioFormat{
			Encoding: media.AudioEncodingPCM16,
			Channels: 1,
		}
	default:
		return media.AudioFormat{
			Encoding: media.AudioEncoding(string(params.ResponseFormat)),
			Channels: 1,
		}
	}
}

// ---------------------------------------------------------------------------
// Unary: drain the audio body into one payload.
// ---------------------------------------------------------------------------

func transportTTS(
	client openai.Client,
) inference.Transport[openai.AudioSpeechNewParams, ttsRaw] {
	return func(
		ctx context.Context,
		params openai.AudioSpeechNewParams,
	) (ttsRaw, error) {
		body, err := client.Audio.Speech.New(ctx, params)
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
		return ttsRaw{data: data, format: ttsCanonicalFormat(params)}, nil
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
	var duration *int64
	if millis, ok := media.AudioDurationMillis(raw.data, raw.format); ok {
		duration = &millis
	}
	return inference.GenerateResponse{
		Message: message.Message{
			Role: message.RoleAssistant,
			Content: message.Content{Parts: []message.Part{
				message.AudioPart{
					Source:         source,
					Format:         &format,
					DurationMillis: duration,
				},
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
) inference.Transport[openai.AudioSpeechNewParams, inference.ProviderStream[ttsStreamRaw]] {
	return func(
		ctx context.Context,
		params openai.AudioSpeechNewParams,
	) (inference.ProviderStream[ttsStreamRaw], error) {
		body, err := client.Audio.Speech.New(ctx, params)
		if err != nil {
			return nil, classifyError(err)
		}
		return &ttsStream{
			body:   body.Body,
			format: ttsCanonicalFormat(params),
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
	id model.ModelID,
	_ string,
) (inference.GenerateOperations, error) {
	return inference.BindGenerateOperations(
		compileTTS(id.Name),
		transportTTS(cls.api),
		decodeTTS,
		transportTTSStream(cls.api),
		decodeTTSStream,
	)
}
