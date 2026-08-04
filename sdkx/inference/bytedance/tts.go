package bytedance

import (
	"context"
	"fmt"
	"io"
	"iter"
	"math"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/message/media"

	doubaospeech "github.com/GizClaw/doubao-speech-go"
)

// Speech synthesis runs on the Doubao TTS V2 HTTP streaming endpoint. The
// request's text is spoken as-is; the provider's speech_rate control is a
// percentage offset in [-50, 100], so only canonical speeds in [0.5, 2.0]
// have a truthful mapping — faster or slower requests are rejected instead
// of clamped.

type ttsWire struct {
	text       string
	speaker    string
	language   string
	format     string // doubao audio format token
	sampleRate int
	speechRate int // percentage offset: 0 = 1.0x
	resourceID string
	stream     bool
	// Extension settings (TTSOptions).
	pitchRate  *int
	volumeRate *int
	emotion    string
	bitRate    *int
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
			resourceID: endpoint,
			stream:     shape == inference.GenerateExecutionStream,
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
		if audio := intent.Audio; audio != nil {
			if audio.Voice.ID == "" {
				ledger.reject(
					inference.FieldGenerateIntentAudioVoice,
					"speech synthesis requires a voice",
				)
			}
			wire.speaker = audio.Voice.ID
			wire.language = audio.Voice.Language
			compileTTSFormat(&wire, audio.Format, ledger)
			if audio.Speed != nil {
				speed := *audio.Speed
				if speed < 0.5 || speed > 2.0 {
					ledger.reject(
						inference.FieldGenerateIntentAudioSpeed,
						fmt.Sprintf("provider supports speed between 0.5 and 2, not %g", speed),
					)
				} else {
					wire.speechRate = int(math.Round((speed - 1) * 100))
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
		options, other := operationExtensions[TTSOptions](request.Extensions)
		rejectOtherExtensions("speech synthesis", other, ledger)
		compileTTSOptions(&wire, options, ledger)

		report := ledger.report()
		if len(ledger.order) > 0 {
			return inference.Compiled[ttsWire]{Report: report}, ledger.err()
		}
		return inference.Compiled[ttsWire]{Wire: wire, Report: report}, nil
	}
}

// compileTTSOptions lowers TTSOptions onto the wire. Bitrate applies to
// compressed output only; raw PCM has no bitrate channel.
func compileTTSOptions(
	wire *ttsWire,
	options TTSOptions,
	ledger *ledger,
) {
	field := func(name string) inference.FieldID {
		return inference.ExtensionField(name).Qualify(options)
	}
	if options.PitchRate != nil {
		wire.pitchRate = options.PitchRate
	}
	if options.VolumeRate != nil {
		wire.volumeRate = options.VolumeRate
	}
	if options.Emotion != "" {
		wire.emotion = options.Emotion
	}
	if options.BitRate != nil {
		if wire.format == string(doubaospeech.FormatPCM) {
			ledger.reject(
				field("bit_rate"),
				"bitrate applies to compressed output, not raw PCM",
			)
		} else {
			wire.bitRate = options.BitRate
		}
	}
}

// compileTTSFormat maps the canonical audio format onto doubao tokens.
// Supported rates are the SDK's published set; anything else is rejected so
// the provider never receives a rate it would reinterpret.
func compileTTSFormat(
	wire *ttsWire,
	format media.AudioFormat,
	ledger *ledger,
) {
	switch format.Encoding {
	case media.AudioEncodingPCM16:
		wire.format = string(doubaospeech.FormatPCM)
	case media.AudioEncodingMP3:
		wire.format = string(doubaospeech.FormatMP3)
	case media.AudioEncodingOpus:
		wire.format = string(doubaospeech.FormatOGG)
	case media.AudioEncodingAAC:
		wire.format = string(doubaospeech.FormatAAC)
	default:
		ledger.reject(
			inference.FieldGenerateIntentAudioFormatEncoding,
			fmt.Sprintf("audio encoding %q has no native token", format.Encoding),
		)
		return
	}
	if format.SampleRateHz != 0 {
		switch format.SampleRateHz {
		case 8000, 16000, 22050, 24000, 32000, 44100, 48000:
			wire.sampleRate = format.SampleRateHz
		default:
			ledger.reject(
				inference.FieldGenerateIntentAudioFormatSampleRate,
				fmt.Sprintf("sample rate %d is outside the provider's published set", format.SampleRateHz),
			)
			return
		}
	}
	if format.Channels > 1 {
		ledger.reject(
			inference.FieldGenerateIntentAudioFormatChannels,
			"speech synthesis is mono",
		)
		return
	}
	wire.canonicalEncoding = string(format.Encoding)
	wire.canonicalMediaType = format.Encoding.MediaType()
	wire.canonicalChannels = format.Channels
}

// ---------------------------------------------------------------------------
// Unary: drain the chunk stream into one audio payload.
// ---------------------------------------------------------------------------

func openTTS(
	cls *clients,
	spec Spec,
	id inference.ModelID,
	profile string,
) (inference.GenerateOperations, error) {
	speech, err := cls.requireSpeech(profile)
	if err != nil {
		return inference.GenerateOperations{}, err
	}
	return inference.BindGenerateOperations(
		compileTTS(cls.endpoint(id.Name)),
		transportTTS(speech),
		decodeTTS,
		transportTTSStream(speech),
		decodeTTSStream,
	)
}

func transportTTS(
	client *doubaospeech.Client,
) inference.Transport[ttsWire, ttsRaw] {
	return func(ctx context.Context, wire ttsWire) (ttsRaw, error) {
		raw := ttsRaw{format: ttsCanonicalFormat(wire)}
		for chunk, err := range client.TTSV2.Stream(ctx, ttsRequest(wire)) {
			if err != nil {
				return ttsRaw{}, classifyError(err)
			}
			if failure := ttsChunkError(chunk); failure != nil {
				return ttsRaw{}, failure
			}
			raw.data = append(raw.data, chunk.Audio...)
		}
		if len(raw.data) == 0 {
			return ttsRaw{}, fmt.Errorf("bytedance: synthesis produced no audio")
		}
		return raw, nil
	}
}

func ttsRequest(wire ttsWire) *doubaospeech.TTSV2Request {
	request := &doubaospeech.TTSV2Request{
		Text:       wire.text,
		Speaker:    wire.speaker,
		Format:     doubaospeech.AudioFormat(wire.format),
		SpeechRate: wire.speechRate,
		ResourceID: wire.resourceID,
	}
	if wire.language != "" {
		request.Language = wire.language
	}
	if wire.sampleRate != 0 {
		request.SampleRate = doubaospeech.SampleRate(wire.sampleRate)
	}
	if wire.pitchRate != nil {
		request.PitchRate = *wire.pitchRate
	}
	if wire.volumeRate != nil {
		request.VolumeRate = *wire.volumeRate
	}
	if wire.emotion != "" {
		request.Emotion = wire.emotion
	}
	if wire.bitRate != nil {
		request.BitRate = *wire.bitRate
	}
	return request
}

// ttsV2CodeStreamDone is the provider's terminal-success chunk code.
const ttsV2CodeStreamDone = 20000000

// ttsChunkError guards chunk codes the SDK passes through: error codes arrive
// on the iterator's error channel already, so a non-zero, non-terminal code
// here is unexpected but still classified rather than swallowed.
func ttsChunkError(chunk *doubaospeech.TTSV2Chunk) error {
	if chunk == nil {
		return fmt.Errorf("bytedance: empty synthesis chunk")
	}
	if chunk.Code == 0 || chunk.Code == ttsV2CodeStreamDone {
		return nil
	}
	return classifyResponseError(fmt.Sprintf("%d", chunk.Code), chunk.Message)
}

// ttsCanonicalFormat rebuilds the negotiated canonical format from the wire.
// Raw encodings (pcm16) must echo sample rate and channels to stay a valid
// canonical AudioFormat; the synthesis output is mono, so an unset channel
// count resolves to one.
func ttsCanonicalFormat(wire ttsWire) media.AudioFormat {
	format := media.AudioFormat{
		Encoding: media.AudioEncoding(wire.canonicalEncoding),
	}
	if wire.sampleRate != 0 {
		format.SampleRateHz = wire.sampleRate
	}
	format.Channels = wire.canonicalChannels
	switch format.Encoding {
	case media.AudioEncodingPCM16, media.AudioEncodingPCM24, media.AudioEncodingFloat32:
		if format.Channels == 0 {
			format.Channels = 1
		}
	}
	return format
}

func decodeTTS(
	_ context.Context,
	raw ttsRaw,
) (inference.GenerateResponse, error) {
	source, err := media.NewAudioBytes(raw.data, raw.format.Encoding.MediaType())
	if err != nil {
		return inference.GenerateResponse{}, fmt.Errorf("bytedance: audio payload: %w", err)
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
// Stream: every HTTP chunk becomes one audio delta.
// ---------------------------------------------------------------------------

// ttsStream drains the SDK's chunk iterator. Event order is: zero or more
// audio deltas, one finish event, then EOF — the runtime requires exactly
// that terminal sequence.
type ttsStream struct {
	pull   func() (*doubaospeech.TTSV2Chunk, error, bool)
	stop   func()
	format media.AudioFormat

	emitFinish bool // final audio chunk delivered; finish event pending
	done       bool // finish delivered; next Next returns EOF
}

func transportTTSStream(
	client *doubaospeech.Client,
) inference.Transport[ttsWire, inference.ProviderStream[ttsStreamRaw]] {
	return func(
		ctx context.Context,
		wire ttsWire,
	) (inference.ProviderStream[ttsStreamRaw], error) {
		pull, stop := iter.Pull2(client.TTSV2.Stream(ctx, ttsRequest(wire)))
		return &ttsStream{
			pull:   pull,
			stop:   stop,
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
		chunk, err, ok := s.pull()
		if err != nil {
			return ttsStreamRaw{}, classifyError(err)
		}
		if !ok {
			if !s.done {
				return ttsStreamRaw{}, fmt.Errorf(
					"bytedance: synthesis stream ended without a final chunk",
				)
			}
			return ttsStreamRaw{}, io.EOF
		}
		if failure := ttsChunkError(chunk); failure != nil {
			return ttsStreamRaw{}, failure
		}
		if chunk.IsLast {
			s.emitFinish = true
		}
		if len(chunk.Audio) == 0 {
			continue // progress-only line, or a silent final chunk
		}
		format := s.format
		return ttsStreamRaw{data: chunk.Audio, format: &format}, nil
	}
}

func (s *ttsStream) Close() error {
	s.stop()
	return nil
}

// decodeTTSStream turns chunks into audio deltas. It is pure: the negotiated
// format arrives on every raw delta, and the runtime accepts repeated
// identical formats.
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
			"bytedance: synthesis chunk carried no audio",
		)
	}
	format := *raw.format
	return inference.GenerateStreamEvent{
		Delta: inference.AudioPartDelta{Data: raw.data, Format: &format},
	}, nil
}
