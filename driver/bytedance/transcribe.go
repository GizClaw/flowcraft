package bytedance

import (
	"context"
	"fmt"
	"io"
	"iter"
	"sync"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message/media"

	doubaospeech "github.com/GizClaw/doubao-speech-go"
)

// Speech recognition runs on the Doubao ASR V2 SAUC WebSocket endpoint
// (ASRV2.OpenStreamSession). The upstream API is bidirectional streaming
// only — there is no unary HTTP recognition — so the unary contract is
// served by opening a session, feeding the complete inline audio in
// fixed-size packets (final packet flagged), and draining the accumulated
// result inside the transport.
//
// Session semantics are continuous: partial and final events flow for as
// many utterances as the caller sends. When the caller has no more audio,
// FinishInput sends the final negative packet; the provider session then
// reports the resulting final event, io.EOF, and closes the wire session.
// TranscribeStream and the script-bridge finish() drive this lifecycle, so
// one-shot and live flows both terminate deterministically.

const (
	// asrChunkSize paces unary whole-file recognition. At the SDK's
	// recommended 16 kHz mono PCM profile this is ~100 ms per packet.
	asrChunkSize = 3200
	// asrResultTypeFull asks for full-result frames: every server response
	// carries the accumulated hypothesis plus utterance timing.
	asrResultTypeFull = "full"
)

// transcribeWire is the shared provider request for both transcription
// shapes. audio is set by the unary compiler; the session compiler carries
// format/sample-rate/channel/bits instead.
type transcribeWire struct {
	resourceID string
	format     doubaospeech.AudioFormat
	sampleRate doubaospeech.SampleRate
	channel    int
	bits       int
	language   doubaospeech.Language
	audio      []byte
}

// transcribeRaw is the unary transport's aggregated provider result.
type transcribeRaw struct {
	text       string
	utterances []doubaospeech.ASRV2Utterance
	duration   int
	requestID  string
}

// compileTranscribe lowers a whole-file transcription request. The audio
// must be inline: URL sources are not fetched in v1 and stream sources are
// rejected by the canonical request validation anyway.
func compileTranscribe(
	endpoint string,
) inference.Compiler[inference.TranscriptionRequest, transcribeWire] {
	return func(
		_ context.Context,
		_ inference.ModelRef,
		request inference.TranscriptionRequest,
	) (inference.Compiled[transcribeWire], error) {
		ledger := newLedger(
			inference.OperationTranscription,
			request.ActiveFields(),
		)
		wire := transcribeWire{resourceID: endpoint}
		compileTranscribeControls(&wire, request.Language, request.Prompt, ledger)
		for _, field := range request.Extensions.ActiveFields() {
			ledger.reject(field, "bytedance transcription supports no extensions")
		}
		switch request.Audio.Kind() {
		case media.SourceInline:
			wire.audio = request.Audio.Bytes()
			format, err := asrFormatForMediaType(
				request.Audio.BaseMediaType(),
			)
			if err != nil {
				ledger.reject(inference.FieldTranscriptionAudio, err.Error())
			} else {
				wire.format = format
			}
		case media.SourceURL:
			ledger.reject(
				inference.FieldTranscriptionAudio,
				"bytedance ASR needs inline audio; URL sources are not fetched",
			)
		case media.SourceStream:
			ledger.reject(
				inference.FieldTranscriptionAudio,
				"whole-file transcription rejects live stream audio; use TranscribeSession",
			)
		}

		report := ledger.report()
		if len(ledger.order) > 0 {
			return inference.Compiled[transcribeWire]{Report: report}, ledger.err()
		}
		return inference.Compiled[transcribeWire]{Wire: wire, Report: report}, nil
	}
}

// compileTranscribeSession lowers the open-time session request: the
// canonical input format maps onto the ASR audio config, and the remaining
// controls share the unary handling.
func compileTranscribeSession(
	endpoint string,
) inference.Compiler[inference.TranscriptionSessionRequest, transcribeWire] {
	return func(
		_ context.Context,
		_ inference.ModelRef,
		request inference.TranscriptionSessionRequest,
	) (inference.Compiled[transcribeWire], error) {
		ledger := newLedger(
			inference.OperationTranscription,
			request.ActiveFields(),
		)
		wire := transcribeWire{resourceID: endpoint}
		compileTranscribeControls(&wire, request.Language, request.Prompt, ledger)
		for _, field := range request.Extensions.ActiveFields() {
			ledger.reject(field, "bytedance transcription supports no extensions")
		}
		compileASRFormat(&wire, request.InputFormat, ledger)

		report := ledger.report()
		if len(ledger.order) > 0 {
			return inference.Compiled[transcribeWire]{Report: report}, ledger.err()
		}
		return inference.Compiled[transcribeWire]{Wire: wire, Report: report}, nil
	}
}

// compileTranscribeControls maps the canonical language and prompt controls
// shared by both transcription shapes. Timestamps are native (utterances and
// words always carry timing) and need no wire field.
func compileTranscribeControls(
	wire *transcribeWire,
	language string,
	prompt string,
	ledger *ledger,
) {
	if prompt != "" {
		ledger.reject(
			inference.FieldTranscriptionPrompt,
			"bytedance ASR has no prompt control",
		)
	}
	if language == "" {
		return
	}
	lang, ok := asrLanguage(language)
	if !ok {
		ledger.reject(
			inference.FieldTranscriptionLanguage,
			fmt.Sprintf("unsupported language %q", language),
		)
		return
	}
	wire.language = lang
}

// asrLanguage maps canonical language strings to the SDK's published set.
func asrLanguage(language string) (doubaospeech.Language, bool) {
	switch language {
	case "zh-CN", "en-US", "ja-JP", "ko-KR":
		return doubaospeech.Language(language), true
	default:
		return "", false
	}
}

// compileASRFormat maps the canonical session input format onto the ASR
// audio config. Raw PCM carries its sample rate, channel count, and bit
// depth explicitly; compressed encodings are self-describing upstream, so
// absent rates/channels resolve to the SDK defaults.
func compileASRFormat(
	wire *transcribeWire,
	format media.AudioFormat,
	ledger *ledger,
) {
	switch format.Encoding {
	case media.AudioEncodingPCM16:
		wire.format = doubaospeech.FormatPCM
		wire.bits = 16
	case media.AudioEncodingMP3:
		wire.format = doubaospeech.FormatMP3
	case media.AudioEncodingOpus:
		wire.format = doubaospeech.FormatOGG
	case media.AudioEncodingAAC:
		wire.format = doubaospeech.FormatAAC
	default:
		ledger.reject(
			inference.FieldTranscriptionInputFormat,
			fmt.Sprintf("audio encoding %q has no native ASR token", format.Encoding),
		)
		return
	}
	if format.SampleRateHz != 0 {
		switch format.SampleRateHz {
		case 8000, 16000, 22050, 24000, 32000, 44100, 48000:
			wire.sampleRate = doubaospeech.SampleRate(format.SampleRateHz)
		default:
			ledger.reject(
				inference.FieldTranscriptionInputFormat,
				fmt.Sprintf(
					"sample rate %d is outside the provider's published set",
					format.SampleRateHz,
				),
			)
			return
		}
	}
	if format.Channels > 2 {
		ledger.reject(
			inference.FieldTranscriptionInputFormat,
			fmt.Sprintf(
				"provider supports 1 or 2 channels, not %d",
				format.Channels,
			),
		)
		return
	}
	wire.channel = format.Channels
}

// asrFormatForMediaType derives the ASR wire format from a whole-file audio
// source's media type. Raw PCM cannot carry its sample rate or channel count
// through the canonical request, so unary PCM assumes the SDK defaults
// (16 kHz, 16-bit, mono).
func asrFormatForMediaType(mediaType string) (doubaospeech.AudioFormat, error) {
	switch mediaType {
	case "audio/pcm":
		return doubaospeech.FormatPCM, nil
	case "audio/mpeg":
		return doubaospeech.FormatMP3, nil
	case "audio/opus":
		return doubaospeech.FormatOGG, nil
	case "audio/aac":
		return doubaospeech.FormatAAC, nil
	case "audio/wav", "audio/x-wav", "audio/wave":
		return doubaospeech.FormatWAV, nil
	default:
		return "", fmt.Errorf("media type %q has no native ASR token", mediaType)
	}
}

// transportTranscribe serves the unary contract on the streaming wire: open
// an ASR V2 session, feed the complete audio in fixed packets with the final
// packet flagged, and return once the final result arrives.
func transportTranscribe(
	client *doubaospeech.Client,
) inference.Transport[transcribeWire, transcribeRaw] {
	return func(
		ctx context.Context,
		wire transcribeWire,
	) (transcribeRaw, error) {
		if len(wire.audio) == 0 {
			return transcribeRaw{}, fmt.Errorf("bytedance: transcription audio is empty")
		}
		session, err := client.ASRV2.OpenStreamSession(
			ctx,
			asrConfig(wire),
		)
		if err != nil {
			return transcribeRaw{}, classifyError(err)
		}
		defer func() { _ = session.Close() }()

		for offset := 0; offset < len(wire.audio); offset += asrChunkSize {
			end := min(offset+asrChunkSize, len(wire.audio))
			isLast := end == len(wire.audio)
			if err := session.SendAudio(
				ctx,
				wire.audio[offset:end],
				isLast,
			); err != nil {
				return transcribeRaw{}, classifyError(err)
			}
		}

		raw := transcribeRaw{}
		for result, err := range session.Recv() {
			if err != nil {
				return transcribeRaw{}, classifyError(err)
			}
			if result == nil {
				continue
			}
			raw = transcribeRaw{
				text:       result.Text,
				utterances: result.Utterances,
				duration:   result.Duration,
				requestID:  result.ReqID,
			}
			if result.IsFinal {
				return raw, nil
			}
		}
		if raw.text == "" && len(raw.utterances) == 0 {
			return transcribeRaw{}, fmt.Errorf(
				"bytedance: transcription produced no result",
			)
		}
		return raw, nil
	}
}

// transportTranscribeSession opens the provider-native duplex session.
func transportTranscribeSession(
	client *doubaospeech.Client,
) inference.TranscriptionSessionTransport[transcribeWire, *doubaospeech.ASRV2Result] {
	return func(
		ctx context.Context,
		wire transcribeWire,
	) (inference.ProviderSession[*doubaospeech.ASRV2Result], error) {
		session, err := client.ASRV2.OpenStreamSession(
			ctx,
			asrConfig(wire),
		)
		if err != nil {
			return nil, classifyError(err)
		}
		pull, stop := iter.Pull2(session.Recv())
		return &asrProviderSession{
			sdk:       session,
			pull:      pull,
			stop:      stop,
			sendAudio: session.SendAudio,
		}, nil
	}
}

// asrConfig renders the SDK session configuration from the compiled wire.
func asrConfig(wire transcribeWire) *doubaospeech.ASRV2Config {
	return &doubaospeech.ASRV2Config{
		Format:     wire.format,
		SampleRate: wire.sampleRate,
		Channel:    wire.channel,
		Bits:       wire.bits,
		Language:   wire.language,
		ResourceID: wire.resourceID,
		ResultType: asrResultTypeFull,
	}
}

// asrProviderSession adapts the SDK session to the canonical provider
// session. It stays open across final results for continuous recognition;
// FinishInput marks the end of input and the next final result ends the
// session (io.EOF) and closes the underlying WebSocket so callers that never
// call Close (TranscribeStream) do not leak it.
type asrProviderSession struct {
	sdk  *doubaospeech.ASRV2Session
	pull func() (*doubaospeech.ASRV2Result, error, bool)
	stop func()
	// sendAudio is the SDK session's SendAudio; injectable for tests.
	sendAudio func(context.Context, []byte, bool) error

	mu        sync.Mutex
	done      bool
	endedErr  error
	finished  bool // FinishInput called: no more audio will arrive
	finalSeen bool // post-finish final delivered; next Next returns EOF
}

func (s *asrProviderSession) Send(
	ctx context.Context,
	chunk media.AudioChunk,
) error {
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return fmt.Errorf("bytedance: session input already finished")
	}
	s.mu.Unlock()
	if err := s.sdk.SendAudio(ctx, chunk.Data, false); err != nil {
		return classifyError(err)
	}
	return nil
}

// FinishInput sends the final negative audio packet. The server responds
// with the accumulated final result and closes the session; the next final
// event is delivered, then Next returns io.EOF.
func (s *asrProviderSession) FinishInput(ctx context.Context) error {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return fmt.Errorf("bytedance: transcription session ended")
	}
	if s.finished {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	// Empty payload with isLast=true encodes the final negative packet.
	if err := s.sendAudio(ctx, nil, true); err != nil {
		return classifyError(err)
	}
	s.mu.Lock()
	s.finished = true
	s.mu.Unlock()
	return nil
}

func (s *asrProviderSession) Next(
	ctx context.Context,
) (*doubaospeech.ASRV2Result, error) {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		if s.endedErr != nil {
			return nil, s.endedErr
		}
		return nil, io.EOF
	}
	if s.finalSeen {
		s.done = true
		_ = s.closeWireLocked()
		s.mu.Unlock()
		return nil, io.EOF
	}
	s.mu.Unlock()

	for {
		result, err, ok := s.pull()
		if err != nil {
			classified := classifyError(err)
			s.finish(classified)
			return nil, classified
		}
		if !ok {
			s.finish(nil)
			return nil, io.EOF
		}
		if result == nil {
			continue // intermediate control frame
		}
		if result.IsFinal {
			s.mu.Lock()
			if s.finished {
				s.finalSeen = true
				s.mu.Unlock()
				return result, nil
			}
			s.mu.Unlock()
		}
		return result, nil
	}
}

func (s *asrProviderSession) Interrupt() error {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return nil
	}
	s.done = true
	_ = s.closeWireLocked()
	s.mu.Unlock()
	return nil
}

func (s *asrProviderSession) Close() error {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return nil
	}
	s.done = true
	err := s.closeWireLocked()
	s.mu.Unlock()
	if err != nil {
		return classifyError(err)
	}
	return nil
}

// finish records the terminal state for Next. err nil means normal EOF.
func (s *asrProviderSession) finish(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return
	}
	s.done = true
	s.endedErr = err
	_ = s.closeWireLocked()
}

// closeWireLocked releases the pull iterator and the WebSocket. Callers hold
// s.mu. The SDK close is idempotent and best-effort here: a one-shot end
// must not surface close noise as a session error.
func (s *asrProviderSession) closeWireLocked() error {
	if s.stop != nil {
		s.stop()
		s.stop = nil
	}
	if s.sdk == nil {
		return nil
	}
	return s.sdk.Close()
}

// decodeTranscribe maps the unary provider result onto the canonical
// response. Utterances become segments with word-level timing; the joined
// transcript text echoes the provider's accumulated hypothesis.
func decodeTranscribe(
	_ context.Context,
	raw transcribeRaw,
) (inference.TranscriptionResponse, error) {
	segments := make(
		[]inference.TranscriptionSegment,
		0,
		len(raw.utterances),
	)
	for _, utterance := range raw.utterances {
		segment := inference.TranscriptionSegment{
			Text:        utterance.Text,
			StartMillis: int64(utterance.StartTime),
			EndMillis:   int64(utterance.EndTime),
		}
		for _, word := range utterance.Words {
			segment.Words = append(segment.Words, inference.TranscriptionWord{
				Word:        word.Text,
				StartMillis: int64(word.StartTime),
				EndMillis:   int64(word.EndTime),
			})
		}
		segments = append(segments, segment)
	}
	response := inference.TranscriptionResponse{
		Text:     raw.text,
		Segments: segments,
		Metadata: inference.Metadata{RequestID: raw.requestID},
	}
	if raw.duration > 0 {
		duration := int64(raw.duration)
		response.DurationMillis = &duration
	}
	return response, nil
}

// decodeTranscribeSessionEvent maps one provider result onto a canonical
// session event: partials carry the current hypothesis, the final result
// carries the completed utterance as a segment with word timing.
func decodeTranscribeSessionEvent(
	_ context.Context,
	raw *doubaospeech.ASRV2Result,
) (inference.TranscriptionSessionEvent, error) {
	if raw == nil {
		return inference.TranscriptionSessionEvent{}, fmt.Errorf(
			"bytedance: empty ASR session result",
		)
	}
	event := inference.TranscriptionSessionEvent{
		Text:      raw.Text,
		RequestID: raw.ReqID,
	}
	if !raw.IsFinal || len(raw.Utterances) == 0 {
		return event, nil
	}
	utterance := raw.Utterances[len(raw.Utterances)-1]
	segment := inference.TranscriptionSegment{
		Text:        utterance.Text,
		StartMillis: int64(utterance.StartTime),
		EndMillis:   int64(utterance.EndTime),
	}
	for _, word := range utterance.Words {
		segment.Words = append(segment.Words, inference.TranscriptionWord{
			Word:        word.Text,
			StartMillis: int64(word.StartTime),
			EndMillis:   int64(word.EndTime),
		})
	}
	// Providers choose one event style, never both: a final segment replaces
	// the hypothesis text on the event.
	return inference.TranscriptionSessionEvent{
		Final:      true,
		Segment:    &segment,
		RequestID:  raw.ReqID,
		ResponseID: raw.ConnectID,
	}, nil
}

// openASR materializes the unary and duplex transcription drivers for one
// ASR model. The wire address resolves through the profile endpoints map,
// exactly like TTS resource IDs.
func openASR(
	cls *clients,
	id inference.ModelID,
	profile string,
) (inference.TranscribeOperations, error) {
	speech, err := cls.requireSpeech(profile)
	if err != nil {
		return inference.TranscribeOperations{}, err
	}
	endpoint := cls.endpoint(id.Name)
	return inference.BindTranscribeOperations(
		compileTranscribe(endpoint),
		transportTranscribe(speech),
		decodeTranscribe,
		compileTranscribeSession(endpoint),
		transportTranscribeSession(speech),
		decodeTranscribeSessionEvent,
	)
}
