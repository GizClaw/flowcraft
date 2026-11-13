package minimax

import (
	"bufio"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/goccy/go-json"
)

// Speech synthesis runs on the t2a_v2 endpoint: one JSON request, audio back as
// hex — unary in one payload, streaming as SSE chunks. The request's text is
// spoken as-is, so every input part must be text.
//
// MiniMax speeds range [0.5, 2], so a requested speed outside that band is
// rejected instead of clamped; pcm output is 16-bit, so only PCM16 maps
// truthfully among the raw encodings.

const ttsPath = "/v1/t2a_v2"

// ttsWire is one compiled speech request.
type ttsWire struct {
	model    string
	text     string
	stream   bool
	voice    string
	language string
	speed    *float64
	// format is the wire token: mp3, pcm, flac, or opus. Empty sends the
	// endpoint default.
	format   string
	sample   int
	channels int
}

// ttsRaw is one finished unary call.
type ttsRaw struct {
	data           []byte
	format         media.AudioFormat
	requestID      string
	characters     int64
	durationMillis int64
}

// ttsFormatToken maps a canonical encoding onto the wire token.
func ttsFormatToken(encoding media.AudioEncoding) (string, bool) {
	switch encoding {
	case media.AudioEncodingMP3:
		return "mp3", true
	case media.AudioEncodingFLAC:
		return "flac", true
	case media.AudioEncodingOpus:
		return "opus", true
	case media.AudioEncodingPCM16:
		return "pcm", true
	default:
		return "", false
	}
}

// ttsEncodingOf maps a wire token back onto a canonical encoding. It is the
// inverse of ttsFormatToken and exists so the response format is derived from
// the request, never from the payload.
func ttsEncodingOf(token string) media.AudioEncoding {
	switch token {
	case "flac":
		return media.AudioEncodingFLAC
	case "opus":
		return media.AudioEncodingOpus
	case "pcm":
		return media.AudioEncodingPCM16
	default:
		return media.AudioEncodingMP3
	}
}

func compileTTS(
	declared ModelSpec,
	name string,
) protocol.Compiler[inference.GenerateRequest, ttsWire] {
	return func(
		_ context.Context,
		request inference.GenerateRequest,
		shape protocol.Shape,
	) (protocol.Compiled[ttsWire], error) {
		if request.Output.Kind() != inference.OutputAudio {
			return protocol.Compiled[ttsWire]{}, inference.NewError(
				inference.UnsupportedFeature, inference.FieldOutput,
				fmt.Errorf("minimax: the speech pipeline serves audio output only"))
		}
		options, err := ttsOptions(request.Extensions)
		if err != nil {
			return protocol.Compiled[ttsWire]{}, err
		}
		if options == nil || options.Voice == "" {
			return protocol.Compiled[ttsWire]{}, inference.NewError(
				inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("minimax: speech synthesis requires tts_options.voice"))
		}
		wire := ttsWire{
			model:  wireModel(name, declared),
			stream: shape == protocol.ShapeStream,
			voice:  options.Voice,
		}

		var text []string
		collect := func(parts []message.Part) error {
			for _, part := range parts {
				value, ok := part.(message.TextPart)
				if !ok {
					return inference.NewError(
						inference.UnsupportedFeature, inference.FieldContent,
						fmt.Errorf("minimax: speech synthesis speaks text, not %s", part.Kind()))
				}
				text = append(text, value.Text)
			}
			return nil
		}
		for _, turn := range request.Context {
			if turn.Role != message.RoleUser {
				return protocol.Compiled[ttsWire]{}, inference.NewError(
					inference.UnsupportedFeature, inference.FieldContext,
					fmt.Errorf("minimax: speech synthesis keeps user context only"))
			}
			if err := collect(turn.Content.Parts); err != nil {
				return protocol.Compiled[ttsWire]{}, err
			}
		}
		if err := collect(request.Input.Content.Parts); err != nil {
			return protocol.Compiled[ttsWire]{}, err
		}
		wire.text = strings.Join(text, "\n")
		if wire.text == "" {
			return protocol.Compiled[ttsWire]{}, inference.NewError(
				inference.InvalidRequest, inference.FieldContent,
				fmt.Errorf("minimax: speech synthesis needs text"))
		}
		if limit := declared.TTS.PromptLimit; limit > 0 && len([]rune(wire.text)) > limit {
			return protocol.Compiled[ttsWire]{}, inference.NewError(
				inference.InvalidRequest, inference.FieldContent,
				fmt.Errorf("minimax: %s speaks at most %d characters, not %d",
					name, limit, len([]rune(wire.text))))
		}

		wire.language = options.Language
		wire.speed = options.Speed
		if err := compileTTSFormat(&wire, options, declared, name); err != nil {
			return protocol.Compiled[ttsWire]{}, err
		}
		return protocol.Compiled[ttsWire]{
			Wire: wire, Fields: request.ActiveFields(),
		}, nil
	}
}

// compileTTSFormat resolves the payload format: the per-call override, else the
// declaration, else the platform default. Raw encodings need a sample rate and a
// channel count from the same two places, because the canonical audio format
// requires them next to the encoding.
func compileTTSFormat(
	wire *ttsWire,
	options *TTSOptions,
	declared ModelSpec,
	name string,
) error {
	encoding := options.Format
	if encoding == "" {
		encoding = declared.TTS.Encoding
	}
	if encoding != "" {
		token, ok := ttsFormatToken(encoding)
		if !ok {
			return inference.NewError(
				inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("minimax: %s cannot encode %s", name, encoding))
		}
		wire.format = token
	}
	wire.sample = options.SampleRateHz
	if wire.sample == 0 {
		wire.sample = declared.TTS.SampleRateHz
	}
	wire.channels = options.Channels
	if wire.channels == 0 {
		wire.channels = declared.TTS.Channels
	}
	if encoding == media.AudioEncodingPCM16 {
		if wire.sample == 0 || wire.channels == 0 {
			return inference.NewError(
				inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("minimax: pcm16 speech requires a sample rate and a channel count"))
		}
	}
	return nil
}

// ttsCanonicalFormat echoes the negotiated format for the response part; it is
// derived from the request, never from provider payloads. The pcm defaults are
// the platform's own, so a request that names none still describes its payload
// truthfully.
func ttsCanonicalFormat(wire ttsWire) media.AudioFormat {
	format := media.AudioFormat{
		Encoding:     ttsEncodingOf(wire.format),
		SampleRateHz: wire.sample,
		Channels:     wire.channels,
	}
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
	language := wire.language
	if language == "" {
		language = "auto"
	}
	return map[string]any{
		"model":          wire.model,
		"text":           wire.text,
		"stream":         wire.stream,
		"output_format":  "hex",
		"voice_setting":  voice,
		"audio_setting":  audio,
		"language_boost": language,
	}
}

// ttsResponse is the unary t2a_v2 envelope.
type ttsResponse struct {
	Data struct {
		Audio string `json:"audio"`
	} `json:"data"`
	// TraceID is the server-assigned session id the docs describe as "used for
	// troubleshooting and support".
	TraceID   string       `json:"trace_id"`
	ExtraInfo ttsExtraInfo `json:"extra_info"`
	BaseResp  baseResp     `json:"base_resp"`
}

// billableCharacters is the character count the platform bills: its own report
// when present, the request text as a fallback. The two are not the same number —
// the live endpoint counts a 12-rune Chinese sentence as 22 — so a deployment that
// meters on this must not use the rune count when the platform spoke.
func billableCharacters(extra ttsExtraInfo, wire ttsWire) int64 {
	if extra.UsageCharacters > 0 {
		return extra.UsageCharacters
	}
	return int64(len([]rune(wire.text)))
}

// ttsExtraInfo is the accounting the platform reports alongside the audio. Where
// the driver would otherwise estimate — billable characters, clip length — this is
// authoritative, and the live endpoint confirms both surfaces send it: unary in the
// response body, streamed on the terminal event.
type ttsExtraInfo struct {
	AudioLength     int64  `json:"audio_length"`
	AudioSampleRate int    `json:"audio_sample_rate"`
	AudioChannel    int    `json:"audio_channel"`
	AudioFormat     string `json:"audio_format"`
	UsageCharacters int64  `json:"usage_characters"`
}

// transportTTS posts one unary request and decodes its hex payload.
func transportTTS(
	client *mediaClient,
) protocol.Transport[inference.GenerateRequest, ttsWire, ttsRaw] {
	return func(
		ctx context.Context,
		_ inference.GenerateRequest,
		wire ttsWire,
	) (ttsRaw, error) {
		var response ttsResponse
		if err := client.postJSON(ctx, ttsPath, ttsRequest(wire), &response); err != nil {
			return ttsRaw{}, err
		}
		if err := response.BaseResp.err("speech synthesis"); err != nil {
			return ttsRaw{}, err
		}
		data, err := hexDecode(response.Data.Audio)
		if err != nil {
			return ttsRaw{}, err
		}
		return ttsRaw{
			data:           data,
			format:         ttsCanonicalFormat(wire),
			requestID:      response.TraceID,
			characters:     billableCharacters(response.ExtraInfo, wire),
			durationMillis: response.ExtraInfo.AudioLength,
		}, nil
	}
}

func decodeTTS(
	_ context.Context,
	request inference.GenerateRequest,
	raw ttsRaw,
) (inference.GenerateResponse, error) {
	source, err := media.NewAudioBytes(raw.data, raw.format.Encoding.MediaType())
	if err != nil {
		return inference.GenerateResponse{}, fmt.Errorf("minimax: audio payload: %w", err)
	}
	part := message.AudioPart{Source: source, Format: raw.format}
	accounting := usage.Usage{
		Work: map[usage.WorkKind]int64{usage.WorkInputCharacters: raw.characters},
	}
	// The platform's own length is exact; the frame-count estimate is the
	// fallback for a response that carries no accounting.
	if raw.durationMillis > 0 {
		part.DurationMillis = raw.durationMillis
		accounting.Work[usage.WorkAudioDurationMillis] = raw.durationMillis
	} else if millis, ok := media.AudioDurationMillis(raw.data, raw.format); ok {
		part.DurationMillis = millis
		accounting.Work[usage.WorkAudioDurationMillis] = millis
	}
	return inference.GenerateResponse{
		Message: message.Message{
			Role:    message.RoleAssistant,
			Content: message.Content{Parts: []message.Part{part}},
		},
		FinishReason: inference.FinishStop,
		Usage:        accounting,
		Metadata: inference.Metadata{
			Model: request.Model.ID, RequestID: raw.requestID,
		},
	}, nil
}

// hexDecode decodes one hex audio payload; MiniMax's t2a API delivers audio as
// a hex string, not base64.
func hexDecode(raw string) ([]byte, error) {
	data, err := hex.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return nil, inference.NewError(
			inference.InvalidProviderResponse, inference.FieldOutput,
			fmt.Errorf("minimax: audio payload is not valid hex: %w", err))
	}
	return data, nil
}

// ---------------------------------------------------------------------------
// Streamed speech: SSE events of the shape
// {"data": {"audio": <hex>, "status": 1|2}, "trace_id": "..."} — status 1
// carries a chunk, status 2 terminates the stream (its audio may still carry
// the final chunk).
// ---------------------------------------------------------------------------

// ttsStreamEvent is one SSE data payload. The terminal event also reports the
// platform's accounting at the top level, which is why the event rather than the
// chunk carries it.
type ttsStreamEvent struct {
	Data struct {
		Audio  string `json:"audio"`
		Status int    `json:"status"`
	} `json:"data"`
	TraceID   string       `json:"trace_id"`
	ExtraInfo ttsExtraInfo `json:"extra_info"`
	BaseResp  baseResp     `json:"base_resp"`
}

// ttsStreamRaw is one wire chunk on its way to the decoder. The negotiated
// format rides every chunk: it is fixed at compile time, flows wire → raw, and
// never passes through the stateless decoder's construction site.
type ttsStreamRaw struct {
	data      []byte
	format    media.AudioFormat
	requestID string
	// characters and durationMillis are the accounting the terminal chunk
	// carries: billable characters and clip length. They are zero on data
	// chunks.
	characters     int64
	durationMillis int64
	// last marks the synthetic terminal chunk: the stream delivered its
	// terminal status and this chunk closes it.
	last bool
}

// ttsStream adapts an SSE body to the protocol's wire stream. It owns the body:
// Close releases it and unblocks the scanner goroutine.
type ttsStream struct {
	body   io.ReadCloser
	events chan ttsStreamEvent
	errs   chan error
	closed chan struct{}
	format media.AudioFormat
	// characters is the fallback billable count (the request text's runes);
	// the platform's own figure wins when the terminal event reports one.
	characters int64
	traceID    string
	extra      ttsExtraInfo

	// emitFinish records that a terminal status was seen and the end chunk is
	// still pending; done records that it was delivered.
	emitFinish bool
	done       bool
	closeOnce  sync.Once
}

func newTTSStream(body io.ReadCloser, format media.AudioFormat, characters int64) *ttsStream {
	stream := &ttsStream{
		body:       body,
		format:     format,
		characters: characters,
		// One slot: the consumer is a pull loop, so read-ahead buys nothing,
		// and each buffered event can hold a whole SSE line.
		events: make(chan ttsStreamEvent, 1),
		errs:   make(chan error, 1),
		closed: make(chan struct{}),
	}
	go stream.pump()
	return stream
}

// terminal builds the synthetic end chunk, carrying the accounting the stream has
// collected. Reporting it here is what lets the assembled Result() match the unary
// path instead of losing the trace id and the billable count.
func (s *ttsStream) terminal() ttsStreamRaw {
	characters := s.characters
	if s.extra.UsageCharacters > 0 {
		characters = s.extra.UsageCharacters
	}
	return ttsStreamRaw{
		format:         s.format,
		requestID:      s.traceID,
		characters:     characters,
		durationMillis: s.extra.AudioLength,
		last:           true,
	}
}

// pump scans SSE payloads off the body and feeds parsed events. It exits when
// the body ends or the caller closes the stream, so an abandoned stream cannot
// leave a goroutine parked on a full channel.
func (s *ttsStream) pump() {
	defer close(s.events)
	err := sseEvents(s.body, func(payload []byte) bool {
		var event ttsStreamEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			s.report(inference.NewError(
				inference.InvalidProviderResponse, inference.FieldStream,
				fmt.Errorf("minimax: audio stream event: %w", err)))
			return false
		}
		select {
		case s.events <- event:
			return true
		case <-s.closed:
			return false
		}
	})
	if err != nil {
		s.report(err)
	}
}

// report publishes a terminal failure unless the caller already closed the
// stream, in which case nobody is reading and the goroutine must exit.
func (s *ttsStream) report(err error) {
	select {
	case s.errs <- err:
	case <-s.closed:
	}
}

// Next follows the pull contract: io.EOF ends the stream cleanly and any other
// error is terminal.
func (s *ttsStream) Next(ctx context.Context) (ttsStreamRaw, error) {
	for {
		if s.emitFinish {
			s.emitFinish = false
			s.done = true
			return s.terminal(), nil
		}
		if s.done {
			return ttsStreamRaw{}, io.EOF
		}
		select {
		case <-ctx.Done():
			return ttsStreamRaw{}, ctx.Err()
		case <-s.closed:
			return ttsStreamRaw{}, io.EOF
		case failure := <-s.errs:
			return ttsStreamRaw{}, failure
		case event, open := <-s.events:
			if !open {
				select {
				case failure := <-s.errs:
					return ttsStreamRaw{}, failure
				default:
				}
				return ttsStreamRaw{}, inference.NewError(
					inference.ProviderTruncated, inference.FieldStream,
					fmt.Errorf("minimax: audio stream ended without a terminal chunk"))
			}
			if failure := event.BaseResp.err("audio stream"); failure != nil {
				return ttsStreamRaw{}, failure
			}
			if event.TraceID != "" {
				s.traceID = event.TraceID
			}
			// The terminal event reports the platform's own accounting; keep the
			// last non-empty report so the end chunk can carry it.
			if event.ExtraInfo.UsageCharacters > 0 {
				s.extra.UsageCharacters = event.ExtraInfo.UsageCharacters
			}
			if event.ExtraInfo.AudioLength > 0 {
				s.extra.AudioLength = event.ExtraInfo.AudioLength
			}
			if event.Data.Status == 2 {
				s.emitFinish = true
			}
			if event.Data.Audio == "" {
				if s.emitFinish {
					// Silent terminal chunk: finish immediately.
					s.emitFinish = false
					s.done = true
					return s.terminal(), nil
				}
				// A progress-only event: read the next one.
				continue
			}
			data, err := hexDecode(event.Data.Audio)
			if err != nil {
				return ttsStreamRaw{}, err
			}
			return ttsStreamRaw{data: data, format: s.format, requestID: s.traceID}, nil
		}
	}
}

// Close releases the body and unblocks the scanner goroutine. It is safe to call
// more than once.
func (s *ttsStream) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.closed)
		err = s.body.Close()
	})
	return err
}

// openTTSStream starts the streaming call and takes ownership of the body.
func openTTSStream(
	client *mediaClient,
) protocol.StreamOpener[inference.GenerateRequest, ttsWire, ttsStreamRaw] {
	return func(
		ctx context.Context,
		_ inference.GenerateRequest,
		wire ttsWire,
	) (protocol.RawStream[ttsStreamRaw], error) {
		body, err := client.postSSE(ctx, ttsPath, ttsRequest(wire))
		if err != nil {
			return nil, err
		}
		return newTTSStream(body, ttsCanonicalFormat(wire),
			int64(len([]rune(wire.text)))), nil
	}
}

// newTTSStreamDecoder maps chunks onto canonical events: every chunk appends to
// content index zero, which is the semantics [inference.StreamChunk] names. The
// format is fixed at compile time, so it arrives on the decoder rather than on
// each chunk.
func newTTSStreamDecoder(
	request inference.GenerateRequest,
) protocol.StreamDecoder[ttsStreamRaw] {
	started := false
	return func(_ context.Context, raw ttsStreamRaw) ([]inference.StreamEvent, error) {
		var events []inference.StreamEvent
		if !started {
			started = true
			// The trace id is only known once the first event arrives, which is
			// why metadata is built here rather than at decoder construction: the
			// assembler takes it from StreamStart alone.
			metadata := inference.Metadata{
				Model: request.Model.ID, RequestID: raw.requestID,
			}
			events = append(events, inference.StreamEvent{
				Kind: inference.StreamStart, Metadata: &metadata,
			})
		}
		if raw.last {
			// Report the accounting before closing: the unary path reports the same
			// facts, and a metering caller must not get different numbers depending
			// on which shape it chose.
			work := map[usage.WorkKind]int64{}
			if raw.characters > 0 {
				work[usage.WorkInputCharacters] = raw.characters
			}
			if raw.durationMillis > 0 {
				work[usage.WorkAudioDurationMillis] = raw.durationMillis
			}
			if len(work) > 0 {
				accounting := usage.Usage{Work: work}
				events = append(events, inference.StreamEvent{
					Kind: inference.StreamUsage, Usage: &accounting,
				})
			}
			return append(events,
				inference.StreamEvent{Kind: inference.StreamEnd, Finish: inference.FinishStop},
			), nil
		}
		if len(raw.data) == 0 {
			return nil, inference.NewError(
				inference.InvalidProviderResponse, inference.FieldStream,
				fmt.Errorf("minimax: audio chunk carried no data"))
		}
		source, err := media.NewAudioBytes(raw.data, raw.format.Encoding.MediaType())
		if err != nil {
			return nil, err
		}
		events = append(events, inference.StreamEvent{
			Kind:  inference.StreamChunk,
			Index: 0,
			Part:  message.AudioPart{Source: source, Format: raw.format},
		})
		return events, nil
	}
}

// sseEvents scans an event-stream body for "data:" payloads, one JSON document
// per event. Blank lines and comment lines are skipped.
func sseEvents(body io.Reader, yield func([]byte) bool) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if !yield([]byte(payload)) {
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("minimax: read event stream: %w", err)
	}
	return nil
}
