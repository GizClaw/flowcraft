package inference

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// TranscriptionRequest is the canonical input for whole-file speech
// recognition. The audio source is mandatory; language, prompt, and
// timestamps are optional provider-neutral controls whose support is decided
// per driver at compile time through the field ledger.
type TranscriptionRequest struct {
	Audio      media.AudioSource `json:"audio"`
	Language   string            `json:"language,omitempty"`
	Prompt     string            `json:"prompt,omitempty"`
	Timestamps bool              `json:"timestamps,omitempty"`
	Extensions Extensions        `json:"-" ledger:"extension"`
}

func (r TranscriptionRequest) Clone() TranscriptionRequest {
	r.Audio = r.Audio.Clone()
	r.Extensions = r.Extensions.Clone()
	return r
}

func (r TranscriptionRequest) Validate() error {
	if err := r.Audio.Validate(); err != nil {
		return err
	}
	if r.Audio.Kind() == media.SourceStream {
		return fmt.Errorf(
			"transcription request audio must be complete: " +
				"use TranscribeSession for stream sources",
		)
	}
	return r.Extensions.Validate()
}

func (r TranscriptionRequest) ActiveFields() []FieldID {
	fields := []FieldID{FieldTranscriptionAudio}
	if r.Language != "" {
		fields = append(fields, FieldTranscriptionLanguage)
	}
	if r.Prompt != "" {
		fields = append(fields, FieldTranscriptionPrompt)
	}
	if r.Timestamps {
		fields = append(fields, FieldTranscriptionTimestamps)
	}
	return r.Extensions.AppendActiveFields(fields)
}

// TranscriptionSegment is one completed utterance of a transcript.
// StartMillis/EndMillis bound the utterance in the session's audio timeline
// when the provider reports timing; Words carries word-level timing when
// available.
type TranscriptionSegment struct {
	Text        string              `json:"text"`
	StartMillis int64               `json:"start_millis,omitempty"`
	EndMillis   int64               `json:"end_millis,omitempty"`
	Words       []TranscriptionWord `json:"words,omitempty"`
}

func (s TranscriptionSegment) Validate() error {
	if s.Text == "" {
		return fmt.Errorf("transcription segment text is required")
	}
	if s.StartMillis < 0 || s.EndMillis < 0 {
		return fmt.Errorf("transcription segment timestamps must not be negative")
	}
	if s.StartMillis != 0 && s.EndMillis != 0 && s.EndMillis < s.StartMillis {
		return fmt.Errorf("transcription segment end precedes start")
	}
	for index, word := range s.Words {
		if err := word.Validate(); err != nil {
			return fmt.Errorf("transcription segment word %d: %w", index, err)
		}
	}
	return nil
}

type TranscriptionWord struct {
	Word        string `json:"word"`
	StartMillis int64  `json:"start_millis,omitempty"`
	EndMillis   int64  `json:"end_millis,omitempty"`
}

func (w TranscriptionWord) Validate() error {
	if w.Word == "" {
		return fmt.Errorf("transcription word text is required")
	}
	if w.StartMillis < 0 || w.EndMillis < 0 {
		return fmt.Errorf("transcription word timestamps must not be negative")
	}
	if w.StartMillis != 0 && w.EndMillis != 0 && w.EndMillis < w.StartMillis {
		return fmt.Errorf("transcription word end precedes start")
	}
	return nil
}

// TranscriptionResponse is the canonical recognition result. Text is the
// joined transcript; Segments carries per-utterance text with optional
// timing; Language reports the request hint or the detected language;
// DurationMillis is the recognized audio duration when the provider reports
// one.
type TranscriptionResponse struct {
	Text           string                 `json:"text"`
	Segments       []TranscriptionSegment `json:"segments,omitempty"`
	Language       string                 `json:"language,omitempty"`
	DurationMillis *int64                 `json:"duration_millis,omitempty"`
	Usage          Usage                  `json:"usage"`
	Metadata       Metadata               `json:"metadata"`
}

func (r TranscriptionResponse) Clone() TranscriptionResponse {
	r.Segments = append([]TranscriptionSegment(nil), r.Segments...)
	r.Usage = r.Usage.Clone()
	r.Metadata = r.Metadata.Clone()
	return r
}

func (r TranscriptionResponse) ValidateFor(request TranscriptionRequest) error {
	if err := validateTranscriptionSegments(r.Segments); err != nil {
		return err
	}
	if r.DurationMillis != nil && *r.DurationMillis < 0 {
		return fmt.Errorf("transcription duration must not be negative")
	}
	return nil
}

func validateTranscriptionSegments(segments []TranscriptionSegment) error {
	for index, segment := range segments {
		if err := segment.Validate(); err != nil {
			return fmt.Errorf("transcription segment %d: %w", index, err)
		}
	}
	return nil
}

// TranscriptionSessionRequest opens a duplex speech-recognition session.
// Audio arrives incrementally after open through TranscriptionSession.Send;
// InputFormat is therefore mandatory and negotiated at open time. Language,
// Prompt, and Timestamps follow the unary semantics.
type TranscriptionSessionRequest struct {
	InputFormat media.AudioFormat `json:"input_format"`
	Language    string            `json:"language,omitempty"`
	Prompt      string            `json:"prompt,omitempty"`
	Timestamps  bool              `json:"timestamps,omitempty"`
	Extensions  Extensions        `json:"-" ledger:"extension"`
}

func (r TranscriptionSessionRequest) Clone() TranscriptionSessionRequest {
	r.Extensions = r.Extensions.Clone()
	return r
}

func (r TranscriptionSessionRequest) Validate() error {
	if err := r.InputFormat.Validate(); err != nil {
		return fmt.Errorf("transcription session input format: %w", err)
	}
	return r.Extensions.Validate()
}

func (r TranscriptionSessionRequest) ActiveFields() []FieldID {
	fields := []FieldID{FieldTranscriptionInputFormat}
	if r.Language != "" {
		fields = append(fields, FieldTranscriptionLanguage)
	}
	if r.Prompt != "" {
		fields = append(fields, FieldTranscriptionPrompt)
	}
	if r.Timestamps {
		fields = append(fields, FieldTranscriptionTimestamps)
	}
	return r.Extensions.AppendActiveFields(fields)
}

// TranscriptionSessionEvent is one canonical session event. Text carries the
// full current utterance hypothesis (partial); Final closes the current
// utterance and commits it to the transcript; Segment is an alternative
// provider style that commits a completed utterance directly. Providers
// choose one style per event, never both.
type TranscriptionSessionEvent struct {
	Text        string                `json:"text,omitempty"`
	Final       bool                  `json:"final,omitempty"`
	Segment     *TranscriptionSegment `json:"segment,omitempty"`
	StartMillis int64                 `json:"start_millis,omitempty"`
	EndMillis   int64                 `json:"end_millis,omitempty"`
	Language    string                `json:"language,omitempty"`
	// Usage is a cumulative session snapshot and replaces the previous
	// snapshot.
	Usage      *Usage `json:"usage,omitempty"`
	RequestID  string `json:"request_id,omitempty"`
	ResponseID string `json:"response_id,omitempty"`
}

func (e TranscriptionSessionEvent) empty() bool {
	return e.Text == "" && !e.Final && e.Segment == nil &&
		e.StartMillis == 0 && e.EndMillis == 0 && e.Language == "" &&
		e.Usage == nil && e.RequestID == "" && e.ResponseID == ""
}

// TranscriptionSession is the canonical duplex recognition session: the
// caller feeds audio chunks in and drains partial/final transcript events
// out. Send and Next may be called concurrently. Next returns io.EOF after
// the provider ends the session normally; Result then yields the final
// transcript. Interrupt terminates the session abnormally (barge-in): after
// Interrupt, Send/Next/Result fail with an interruption error and no Result
// is produced. Close ends the session normally without producing a result by
// itself; callers drain Next to EOF first.
type TranscriptionSession interface {
	Send(context.Context, media.AudioChunk) error
	Next(context.Context) (TranscriptionSessionEvent, error)
	Result() (TranscriptionResponse, error)
	Interrupt() error
	Close() error
}

// TranscriptionSessionFinisher is an optional session capability: after the
// caller has no more audio, FinishInput asks the provider to finalize the
// session so draining Next reaches io.EOF and Result yields the accumulated
// transcript. Providers whose wire protocol ends sessions on its own need
// not implement it — callers detect support with a type assertion, and
// calling FinishInput on a session without the capability is a no-op.
type TranscriptionSessionFinisher interface {
	FinishInput(context.Context) error
}

// TranscriptionSessionDecoder converts provider-native session events into
// canonical TranscriptionSessionEvents. Implementations must support
// concurrent calls; sessions serialize decoder use themselves.
type TranscriptionSessionDecoder[RawEvent any] func(
	context.Context,
	RawEvent,
) (TranscriptionSessionEvent, error)

// ProviderSession is the provider-native bidirectional session opened by a
// transcription transport. Send receives canonical chunks and encodes them
// to the provider wire format; Next returns provider-native events.
type ProviderSession[RawEvent any] interface {
	Send(context.Context, media.AudioChunk) error
	Next(context.Context) (RawEvent, error)
	Interrupt() error
	Close() error
}

// TranscriptionSessionTransport opens a provider-native session. It is the
// sole stage allowed to perform provider I/O for sessions; like every
// transport it must support concurrent calls.
type TranscriptionSessionTransport[Wire, RawEvent any] func(
	context.Context,
	Wire,
) (ProviderSession[RawEvent], error)

type TranscriptionDriver interface {
	Explain(context.Context, ModelRef, TranscriptionRequest) (Explanation, error)
	Execute(context.Context, ModelRef, TranscriptionRequest) (TranscriptionResponse, error)
	inferenceTranscriptionDriver()
}

type TranscriptionSessionDriver interface {
	Explain(context.Context, ModelRef, TranscriptionSessionRequest) (Explanation, error)
	Open(context.Context, ModelRef, TranscriptionSessionRequest) (TranscriptionSession, error)
	inferenceTranscriptionSessionDriver()
}

// TranscribeOperations materializes the unary and/or duplex session drivers
// a transcription model serves. Openers.Transcribe returns it; the assembly
// routes each execution shape to the matching driver.
type TranscribeOperations struct {
	Unary   TranscriptionDriver
	Session TranscriptionSessionDriver
}

type transcribeCompilerBinding struct {
	_ byte
}

type boundTranscribeDriver interface {
	transcribeCompilerBinding() *transcribeCompilerBinding
}

func (o TranscribeOperations) Validate() error {
	if isNilValue(o.Unary) && isNilValue(o.Session) {
		return fmt.Errorf("transcribe operations require a unary or session driver")
	}
	if !isNilValue(o.Unary) && !isNilValue(o.Session) {
		unary, unaryOK := o.Unary.(boundTranscribeDriver)
		session, sessionOK := o.Session.(boundTranscribeDriver)
		if !unaryOK || !sessionOK ||
			unary.transcribeCompilerBinding() != session.transcribeCompilerBinding() {
			return fmt.Errorf(
				"dual transcribe operations must be created by BindTranscribeOperations",
			)
		}
	}
	return nil
}

// BindTranscribe binds one unary transcription pipeline. compile is local
// validation and wire construction only; transport owns provider I/O.
func BindTranscribe[Wire, Raw any](
	compile Compiler[TranscriptionRequest, Wire],
	transport Transport[Wire, Raw],
	decode Decoder[Raw, TranscriptionResponse],
) (TranscriptionDriver, error) {
	return bindTranscribe(
		compile,
		transport,
		decode,
		&transcribeCompilerBinding{},
	)
}

func bindTranscribe[Wire, Raw any](
	compile Compiler[TranscriptionRequest, Wire],
	transport Transport[Wire, Raw],
	decode Decoder[Raw, TranscriptionResponse],
	binding *transcribeCompilerBinding,
) (TranscriptionDriver, error) {
	bound, err := bindPipeline(
		OperationTranscription,
		compile,
		transport,
		decode,
		TranscriptionRequest.Validate,
		TranscriptionRequest.ActiveFields,
		func(request TranscriptionRequest) Extensions { return request.Extensions },
		func(request TranscriptionRequest, extensions Extensions) TranscriptionRequest {
			request.Extensions = extensions
			return request
		},
		TranscriptionRequest.Clone,
		func(request TranscriptionRequest, response TranscriptionResponse) error {
			return response.ValidateFor(request)
		},
	)
	if err != nil {
		return nil, err
	}
	return &transcribeDriver[Wire, Raw]{
		pipeline: bound,
		binding:  binding,
	}, nil
}

// BindTranscribeSession binds one duplex session driver. The shared pipeline
// compiles and validates the session request exactly like unary
// transcription; the transport opens the provider session and the decoder
// normalizes its events.
func BindTranscribeSession[Wire, RawEvent any](
	compile Compiler[TranscriptionSessionRequest, Wire],
	transport TranscriptionSessionTransport[Wire, RawEvent],
	decode TranscriptionSessionDecoder[RawEvent],
) (TranscriptionSessionDriver, error) {
	return bindTranscribeSession(
		compile,
		transport,
		decode,
		&transcribeCompilerBinding{},
	)
}

func bindTranscribeSession[Wire, RawEvent any](
	compile Compiler[TranscriptionSessionRequest, Wire],
	transport TranscriptionSessionTransport[Wire, RawEvent],
	decode TranscriptionSessionDecoder[RawEvent],
	binding *transcribeCompilerBinding,
) (TranscriptionSessionDriver, error) {
	if decode == nil {
		return nil, errdefs.Validationf(
			"inference transcription session requires a decoder",
		)
	}
	// The session shares the compile/validate pipeline with unary
	// transcription. Its decode stage never runs as a unary decoder:
	// events decode per raw event inside the session, not per response.
	bound, err := bindPipeline(
		OperationTranscription,
		compile,
		Transport[Wire, ProviderSession[RawEvent]](transport),
		func(context.Context, ProviderSession[RawEvent]) (TranscriptionResponse, error) {
			return TranscriptionResponse{}, fmt.Errorf(
				"transcription session has no unary decode",
			)
		},
		TranscriptionSessionRequest.Validate,
		TranscriptionSessionRequest.ActiveFields,
		func(request TranscriptionSessionRequest) Extensions {
			return request.Extensions
		},
		func(request TranscriptionSessionRequest, extensions Extensions) TranscriptionSessionRequest {
			request.Extensions = extensions
			return request
		},
		TranscriptionSessionRequest.Clone,
		func(request TranscriptionSessionRequest, response TranscriptionResponse) error {
			return validateSessionResult(response)
		},
	)
	if err != nil {
		return nil, err
	}
	return &transcribeSessionDriver[Wire, RawEvent]{
		pipeline: bound,
		decode:   decode,
		binding:  binding,
	}, nil
}

// BindTranscribeOperations binds unary and session drivers against the same
// wire family so the runtime can prove both shapes were constructed
// together. The shapes compile different request contracts (a complete
// audio source vs an open-time session request), so each takes its own
// compiler while sharing the Wire type and binding.
func BindTranscribeOperations[Wire, Raw, RawEvent any](
	compile Compiler[TranscriptionRequest, Wire],
	transport Transport[Wire, Raw],
	decode Decoder[Raw, TranscriptionResponse],
	sessionCompile Compiler[TranscriptionSessionRequest, Wire],
	sessionTransport TranscriptionSessionTransport[Wire, RawEvent],
	sessionDecode TranscriptionSessionDecoder[RawEvent],
) (TranscribeOperations, error) {
	binding := &transcribeCompilerBinding{}
	unary, err := bindTranscribe(compile, transport, decode, binding)
	if err != nil {
		return TranscribeOperations{}, err
	}
	session, err := bindTranscribeSession(
		sessionCompile,
		sessionTransport,
		sessionDecode,
		binding,
	)
	if err != nil {
		return TranscribeOperations{}, err
	}
	return TranscribeOperations{Unary: unary, Session: session}, nil
}

type transcribeDriver[Wire, Raw any] struct {
	pipeline *pipeline[TranscriptionRequest, Wire, Raw, TranscriptionResponse]
	binding  *transcribeCompilerBinding
}

func (*transcribeDriver[Wire, Raw]) inferenceTranscriptionDriver() {}
func (d *transcribeDriver[Wire, Raw]) transcribeCompilerBinding() *transcribeCompilerBinding {
	return d.binding
}

func (d *transcribeDriver[Wire, Raw]) Explain(
	ctx context.Context,
	model ModelRef,
	request TranscriptionRequest,
) (Explanation, error) {
	return d.pipeline.explain(ctx, model, request)
}

func (d *transcribeDriver[Wire, Raw]) Execute(
	ctx context.Context,
	model ModelRef,
	request TranscriptionRequest,
) (TranscriptionResponse, error) {
	start := time.Now()
	response, report, err := d.pipeline.execute(ctx, model, request)
	if err != nil {
		return TranscriptionResponse{}, err
	}
	response.Usage.Model = model
	response.Usage.LatencyMs = time.Since(start).Milliseconds()
	response.Metadata = mergeProviderIDs(
		report.Metadata(model),
		response.Metadata,
	)
	return response, nil
}

type transcribeSessionDriver[Wire, RawEvent any] struct {
	pipeline *pipeline[
		TranscriptionSessionRequest,
		Wire,
		ProviderSession[RawEvent],
		TranscriptionResponse,
	]
	decode  TranscriptionSessionDecoder[RawEvent]
	binding *transcribeCompilerBinding
}

func (*transcribeSessionDriver[Wire, RawEvent]) inferenceTranscriptionSessionDriver() {}
func (d *transcribeSessionDriver[Wire, RawEvent]) transcribeCompilerBinding() *transcribeCompilerBinding {
	return d.binding
}

func (d *transcribeSessionDriver[Wire, RawEvent]) Explain(
	ctx context.Context,
	model ModelRef,
	request TranscriptionSessionRequest,
) (Explanation, error) {
	return d.pipeline.explain(ctx, model, request)
}

func (d *transcribeSessionDriver[Wire, RawEvent]) Open(
	ctx context.Context,
	model ModelRef,
	request TranscriptionSessionRequest,
) (TranscriptionSession, error) {
	compiled, err := d.pipeline.prepare(ctx, model, request)
	if err != nil {
		return nil, err
	}
	raw, err := d.pipeline.transport(ctx, compiled.Wire)
	if err != nil {
		return nil, newProviderError(
			OperationTranscription,
			model.ID.Provider,
			err,
		)
	}
	if isNilValue(raw) {
		return nil, NewError(
			InvalidProviderResponse,
			OperationTranscription,
			"",
			fmt.Errorf("provider opened a nil transcription session"),
		)
	}
	return &decodedTranscriptionSession[RawEvent]{
		raw:       raw,
		decode:    d.decode,
		model:     model,
		request:   request.Clone(),
		report:    compiled.Report,
		startedAt: time.Now(),
	}, nil
}

type decodedTranscriptionSession[RawEvent any] struct {
	raw     ProviderSession[RawEvent]
	decode  TranscriptionSessionDecoder[RawEvent]
	model   ModelRef
	request TranscriptionSessionRequest
	report  CompileReport

	mu        sync.Mutex
	done      bool
	result    TranscriptionResponse
	resultErr error

	segments     []TranscriptionSegment
	partialText  string
	partialStart *int64
	language     string
	usage        Usage
	requestID    string
	responseID   string
	startedAt    time.Time
}

func (s *decodedTranscriptionSession[RawEvent]) Send(
	ctx context.Context,
	chunk media.AudioChunk,
) error {
	if err := chunk.Validate(); err != nil {
		return NewError(
			InvalidRequest,
			OperationTranscription,
			FieldTranscriptionAudio,
			err,
		)
	}
	s.mu.Lock()
	if s.done {
		err := s.resultErr
		if err == nil {
			err = NewError(
				OperationInterrupted,
				OperationTranscription,
				"",
				fmt.Errorf("transcription session ended"),
			)
		}
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	return s.raw.Send(ctx, chunk)
}

// FinishInput tells the provider no more audio will arrive so it can
// finalize the session; callers then drain Next to io.EOF before Result.
// Providers without the capability keep the call a no-op.
func (s *decodedTranscriptionSession[RawEvent]) FinishInput(
	ctx context.Context,
) error {
	s.mu.Lock()
	if s.done {
		err := s.resultErr
		if err == nil {
			err = NewError(
				OperationInterrupted,
				OperationTranscription,
				"",
				fmt.Errorf("transcription session ended"),
			)
		}
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	finisher, ok := s.raw.(TranscriptionSessionFinisher)
	if !ok {
		return nil
	}
	if err := finisher.FinishInput(ctx); err != nil {
		return newProviderError(
			OperationTranscription,
			s.model.ID.Provider,
			err,
		)
	}
	return nil
}

func (s *decodedTranscriptionSession[RawEvent]) Next(
	ctx context.Context,
) (TranscriptionSessionEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		if s.resultErr != nil {
			return TranscriptionSessionEvent{}, s.resultErr
		}
		return TranscriptionSessionEvent{}, io.EOF
	}
	rawEvent, err := s.raw.Next(ctx)
	if err == io.EOF {
		s.finishResultLocked()
		if s.resultErr != nil {
			return TranscriptionSessionEvent{}, s.resultErr
		}
		return TranscriptionSessionEvent{}, io.EOF
	}
	if err != nil {
		s.done = true
		s.resultErr = newProviderError(
			OperationTranscription,
			s.model.ID.Provider,
			err,
		)
		return TranscriptionSessionEvent{}, s.resultErr
	}
	event, err := s.decode(ctx, rawEvent)
	if err != nil {
		s.done = true
		s.resultErr = NewError(
			InvalidProviderResponse,
			OperationTranscription,
			"",
			err,
		)
		return TranscriptionSessionEvent{}, s.resultErr
	}
	if err := s.accumulate(event); err != nil {
		s.done = true
		s.resultErr = NewError(
			InvalidProviderResponse,
			OperationTranscription,
			"",
			err,
		)
		return TranscriptionSessionEvent{}, s.resultErr
	}
	return event, nil
}

func (s *decodedTranscriptionSession[RawEvent]) Result() (TranscriptionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.done {
		return TranscriptionResponse{}, NewError(
			InvalidProviderResponse,
			OperationTranscription,
			"",
			fmt.Errorf("transcription session is not complete"),
		)
	}
	return s.result.Clone(), s.resultErr
}

func (s *decodedTranscriptionSession[RawEvent]) Interrupt() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return NewError(
			OperationInterrupted,
			OperationTranscription,
			"",
			fmt.Errorf("transcription session already ended"),
		)
	}
	err := s.raw.Interrupt()
	s.done = true
	if err != nil {
		s.resultErr = newProviderError(
			OperationTranscription,
			s.model.ID.Provider,
			err,
		)
		return s.resultErr
	}
	s.resultErr = NewError(
		OperationInterrupted,
		OperationTranscription,
		"",
		fmt.Errorf("transcription session interrupted"),
	)
	return nil
}

func (s *decodedTranscriptionSession[RawEvent]) Close() error {
	if err := s.raw.Close(); err != nil {
		return newProviderError(
			OperationTranscription,
			s.model.ID.Provider,
			err,
		)
	}
	return nil
}

func (s *decodedTranscriptionSession[RawEvent]) accumulate(
	event TranscriptionSessionEvent,
) error {
	if event.empty() {
		return fmt.Errorf("transcription session event is empty")
	}
	if event.StartMillis < 0 || event.EndMillis < 0 {
		return fmt.Errorf("transcription session timestamps must not be negative")
	}
	if event.StartMillis != 0 && event.EndMillis != 0 &&
		event.EndMillis < event.StartMillis {
		return fmt.Errorf("transcription session end precedes start")
	}
	if event.Usage != nil {
		if err := event.Usage.Validate(); err != nil {
			return fmt.Errorf("transcription session usage: %w", err)
		}
	}
	if event.Segment != nil {
		if err := event.Segment.Validate(); err != nil {
			return fmt.Errorf("transcription session segment: %w", err)
		}
	}
	if event.Text != "" {
		s.partialText = event.Text
		if event.StartMillis != 0 {
			start := event.StartMillis
			s.partialStart = &start
		}
	}
	switch {
	case event.Final && event.Segment != nil:
		s.segments = append(s.segments, *event.Segment)
		s.partialText = ""
		s.partialStart = nil
	case event.Final:
		if s.partialText != "" {
			segment := TranscriptionSegment{
				Text:        s.partialText,
				StartMillis: event.StartMillis,
				EndMillis:   event.EndMillis,
			}
			if s.partialStart != nil && segment.StartMillis == 0 {
				segment.StartMillis = *s.partialStart
			}
			s.segments = append(s.segments, segment)
			s.partialText = ""
			s.partialStart = nil
		}
	case event.Segment != nil:
		s.segments = append(s.segments, *event.Segment)
	}
	if event.Language != "" {
		s.language = event.Language
	}
	if event.Usage != nil {
		s.usage = event.Usage.Clone()
	}
	if event.RequestID != "" {
		s.requestID = event.RequestID
	}
	if event.ResponseID != "" {
		s.responseID = event.ResponseID
	}
	return nil
}

func (s *decodedTranscriptionSession[RawEvent]) finishResultLocked() {
	s.done = true
	var text strings.Builder
	for index, segment := range s.segments {
		if index > 0 {
			text.WriteString("\n")
		}
		text.WriteString(segment.Text)
	}
	if s.partialText != "" {
		if text.Len() > 0 {
			text.WriteString("\n")
		}
		text.WriteString(s.partialText)
	}
	response := TranscriptionResponse{
		Text:     text.String(),
		Segments: append([]TranscriptionSegment(nil), s.segments...),
		Language: s.language,
		Usage:    s.usage,
	}
	metadata := s.report.Metadata(s.model)
	metadata.RequestID = s.requestID
	metadata.ResponseID = s.responseID
	response.Metadata = metadata
	response.Usage.Model = s.model
	response.Usage.LatencyMs = time.Since(s.startedAt).Milliseconds()
	if err := validateSessionResult(response); err != nil {
		s.resultErr = NewError(
			InvalidProviderResponse,
			OperationTranscription,
			"",
			err,
		)
		return
	}
	s.result = response
}

func validateSessionResult(response TranscriptionResponse) error {
	if err := validateTranscriptionSegments(response.Segments); err != nil {
		return err
	}
	if response.DurationMillis != nil && *response.DurationMillis < 0 {
		return fmt.Errorf("transcription duration must not be negative")
	}
	return nil
}
