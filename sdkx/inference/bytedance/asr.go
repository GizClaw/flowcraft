package bytedance

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/media"

	doubaospeech "github.com/GizClaw/doubao-speech-go"
)

// Streaming recognition runs on the Doubao ASR V2 SAUC WebSocket session.
// The session accepts 16-bit PCM at 8 or 16 kHz; other encodings and rates
// have no native representation and are rejected at compile time. Language
// is restricted to the provider's published set. A requested language echoes
// back on events and the result with the "requested" source — the provider
// does not report detected languages.

type asrWire struct {
	resourceID string
	format     string // doubao audio format token
	sampleRate int
	channels   int
	bits       int
	language   string
	timestamps bool
	// Extension settings (TranscriptionOptions).
	diarization bool
	speakerNum  int
	hotwords    []string
	resultType  string
	itn         bool
	punc        bool
}

func compileASR(
	endpoint string,
) inference.Compiler[inference.TranscriptionSessionConfig, asrWire] {
	return func(
		_ context.Context,
		model inference.ModelRef,
		config inference.TranscriptionSessionConfig,
	) (inference.Compiled[asrWire], error) {
		ledger := newLedger(
			inference.OperationTranscription,
			config.ActiveFields(),
		)
		wire := asrWire{
			resourceID: endpoint,
			channels:   1,
			bits:       16,
			language:   config.Language,
			timestamps: config.Timestamps == nil || *config.Timestamps,
			itn:        true,
			punc:       true,
		}

		format := config.InputFormat
		switch format.Encoding {
		case media.AudioEncodingPCM16:
			wire.format = string(doubaospeech.FormatPCM)
		default:
			ledger.reject(
				inference.FieldTranscriptionInputFormat,
				fmt.Sprintf("ASR V2 consumes 16-bit PCM, not %q", format.Encoding),
			)
		}
		switch format.SampleRateHz {
		case 8000, 16000:
			wire.sampleRate = format.SampleRateHz
		default:
			ledger.reject(
				inference.FieldTranscriptionInputFormat,
				fmt.Sprintf("ASR V2 accepts 8000 or 16000 Hz, not %d", format.SampleRateHz),
			)
		}
		switch format.Channels {
		case 0, 1:
			wire.channels = 1
		case 2:
			wire.channels = 2
		default:
			ledger.reject(
				inference.FieldTranscriptionInputFormat,
				fmt.Sprintf("ASR V2 accepts mono or stereo, not %d channels", format.Channels),
			)
		}
		switch config.Language {
		case "",
			string(doubaospeech.LanguageZhCN),
			string(doubaospeech.LanguageEnUS),
			string(doubaospeech.LanguageJaJP),
			string(doubaospeech.LanguageKoKR):
		default:
			ledger.reject(
				inference.FieldTranscriptionLanguage,
				fmt.Sprintf("language %q is outside the provider's published set", config.Language),
			)
		}
		if config.Prompt != "" {
			ledger.reject(
				inference.FieldTranscriptionPrompt,
				"ASR V2 has no prompt channel",
			)
		}
		options, other := operationExtensions[TranscriptionOptions](config.Extensions)
		rejectOtherExtensions("transcription", other, ledger)
		compileASROptions(&wire, options, ledger)

		report := ledger.report()
		if len(ledger.order) > 0 {
			return inference.Compiled[asrWire]{Report: report}, ledger.err()
		}
		return inference.Compiled[asrWire]{Wire: wire, Report: report}, nil
	}
}

// compileASROptions lowers TranscriptionOptions onto the wire. A speaker
// count hint implies diarization; pairing it with an explicit diarization
// disable is contradictory and rejected.
func compileASROptions(
	wire *asrWire,
	options TranscriptionOptions,
	ledger *ledger,
) {
	field := func(name string) inference.FieldID {
		return inference.ExtensionField(name).Qualify(options)
	}
	if options.Diarization != nil {
		wire.diarization = *options.Diarization
	}
	if options.SpeakerNum != nil {
		if options.Diarization != nil && !*options.Diarization {
			ledger.reject(
				field("speaker_num"),
				"a speaker count hint requires diarization",
			)
		} else {
			wire.diarization = true
			wire.speakerNum = *options.SpeakerNum
		}
	}
	if len(options.Hotwords) > 0 {
		wire.hotwords = append([]string(nil), options.Hotwords...)
	}
	if options.ResultType != "" {
		wire.resultType = options.ResultType
	}
	if options.ITN != nil {
		wire.itn = *options.ITN
	}
	if options.Punctuation != nil {
		wire.punc = *options.Punctuation
	}
}

func openASRSession(
	client *doubaospeech.Client,
) inference.Transport[asrWire, inference.TranscriptionSession] {
	return func(
		ctx context.Context,
		wire asrWire,
	) (inference.TranscriptionSession, error) {
		config := &doubaospeech.ASRV2Config{
			Format:            doubaospeech.AudioFormat(wire.format),
			SampleRate:        doubaospeech.SampleRate(wire.sampleRate),
			Channel:           wire.channels,
			Bits:              wire.bits,
			Language:          doubaospeech.Language(wire.language),
			EnableITN:         wire.itn,
			EnablePunc:        wire.punc,
			EnableDiarization: wire.diarization,
			SpeakerNum:        wire.speakerNum,
			Hotwords:          wire.hotwords,
			ResultType:        wire.resultType,
			ResourceID:        wire.resourceID,
		}
		session, err := client.ASRV2.OpenStreamSession(ctx, config)
		if err != nil {
			return nil, classifyError(err)
		}
		adapted := &asrSession{
			session: session,
			wire:    wire,
			stop:    make(chan struct{}),
		}
		adapted.results = pumpASRResults(session, adapted.stop)
		if wire.language != "" {
			adapted.language = &inference.TranscriptLanguage{
				Code:   wire.language,
				Source: inference.TranscriptLanguageRequested,
			}
		}
		return adapted, nil
	}
}

// asrSession adapts the SDK session to the canonical TranscriptionSession.
// ITN and punctuation are always enabled so transcripts are presentation
// text; the provider emits no speech boundary events, so only partial and
// final transcript events surface.
type asrSession struct {
	session  *doubaospeech.ASRV2Session
	results  <-chan asrResult
	stop     chan struct{}
	stopOnce sync.Once
	wire     asrWire
	language *inference.TranscriptLanguage

	mu        sync.Mutex
	text      strings.Builder
	segments  []inference.TranscriptSegment
	duration  int64
	inputDone bool
}

type asrResult struct {
	value *doubaospeech.ASRV2Result
	err   error
}

func pumpASRResults(
	session *doubaospeech.ASRV2Session,
	stop <-chan struct{},
) <-chan asrResult {
	results := make(chan asrResult, 1)
	go func() {
		defer close(results)
		for value, err := range session.Recv() {
			select {
			case results <- asrResult{value: value, err: err}:
			case <-stop:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return results
}

func (s *asrSession) SendAudio(
	ctx context.Context,
	chunk media.AudioChunk,
) error {
	return classifyError(s.session.SendAudio(ctx, chunk.Data, false))
}

func (s *asrSession) CloseInput(ctx context.Context) error {
	s.mu.Lock()
	if s.inputDone {
		s.mu.Unlock()
		return fmt.Errorf("bytedance: transcription input already closed")
	}
	s.inputDone = true
	s.mu.Unlock()
	return classifyError(s.session.SendAudio(ctx, nil, true))
}

func (s *asrSession) Next(
	ctx context.Context,
) (inference.TranscriptionEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for {
		var received asrResult
		var ok bool
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case received, ok = <-s.results:
		}
		if !ok {
			return nil, io.EOF
		}
		if received.err != nil {
			return nil, classifyError(received.err)
		}
		result := received.value
		if result == nil {
			continue
		}
		s.mu.Lock()
		if result.Duration > 0 {
			s.duration = int64(result.Duration)
		}
		s.mu.Unlock()
		if result.Text == "" {
			continue
		}
		if !result.IsFinal {
			return inference.PartialTranscriptEvent{
				Text:     result.Text,
				Language: cloneLanguage(s.language),
			}, nil
		}
		segments := s.utteranceSegments(result.Utterances)
		s.mu.Lock()
		s.text.WriteString(result.Text)
		s.segments = append(s.segments, segments...)
		s.mu.Unlock()
		event := inference.FinalTranscriptEvent{
			Text:     result.Text,
			Language: cloneLanguage(s.language),
		}
		if s.wire.timestamps {
			event.Segments = segments
		}
		return event, nil
	}
}

func cloneLanguage(
	language *inference.TranscriptLanguage,
) *inference.TranscriptLanguage {
	if language == nil {
		return nil
	}
	cloned := language.Clone()
	return &cloned
}

// utteranceSegments keeps only definite utterances: tentative utterance text
// is already reflected in the result text and must not double-count.
func (s *asrSession) utteranceSegments(
	utterances []doubaospeech.ASRV2Utterance,
) []inference.TranscriptSegment {
	segments := make([]inference.TranscriptSegment, 0, len(utterances))
	for _, utterance := range utterances {
		if !utterance.Definite {
			continue
		}
		segments = append(segments, inference.TranscriptSegment{
			Text:        utterance.Text,
			Speaker:     utterance.SpeakerID,
			StartMillis: int64(utterance.StartTime),
			EndMillis:   int64(utterance.EndTime),
			Language:    cloneLanguage(s.language),
		})
	}
	return segments
}

func (s *asrSession) Result() (inference.TranscriptionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return inference.TranscriptionResponse{
		Text:     s.text.String(),
		Language: cloneLanguage(s.language),
		Segments: append([]inference.TranscriptSegment(nil), s.segments...),
		Usage: inference.TranscriptionUsage{
			AudioDurationMillis: s.duration,
		},
	}, nil
}

func (s *asrSession) Close() error {
	s.stopOnce.Do(func() { close(s.stop) })
	return classifyError(s.session.Close())
}

func openASR(
	cls *clients,
	spec Spec,
	id inference.ModelID,
	profile string,
) (inference.TranscriptionOperations, error) {
	speech, err := cls.requireSpeech(profile)
	if err != nil {
		return inference.TranscriptionOperations{}, err
	}
	session, err := inference.BindTranscriptionSession(
		compileASR(cls.endpoint(id.Name)),
		openASRSession(speech),
	)
	if err != nil {
		return inference.TranscriptionOperations{}, err
	}
	return inference.TranscriptionOperations{Session: session}, nil
}
