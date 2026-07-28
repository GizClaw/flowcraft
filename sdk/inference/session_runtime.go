package inference

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/inference/media"
)

type OpenedTranscriptionSession interface {
	TranscriptionSession
	Metadata() Metadata
}

type OpenedRealtimeSession interface {
	RealtimeSession
	Metadata() Metadata
	InputReports() []CompileReport
}

type runtimeTranscriptionSession struct {
	model    ModelRef
	config   TranscriptionSessionConfig
	session  TranscriptionSession
	metadata Metadata

	closeOnce sync.Once
	closeErr  error
}

func wrapTranscriptionSession(
	model ModelRef,
	config TranscriptionSessionConfig,
	session TranscriptionSession,
	metadata Metadata,
) OpenedTranscriptionSession {
	base := &runtimeTranscriptionSession{
		model: model, config: config.Clone(), session: session, metadata: cloneMetadata(metadata),
	}
	if committer, ok := session.(TranscriptionCommitter); ok {
		return &runtimeCommittableTranscriptionSession{
			runtimeTranscriptionSession: base,
			committer:                   committer,
		}
	}
	return base
}

type runtimeCommittableTranscriptionSession struct {
	*runtimeTranscriptionSession
	committer TranscriptionCommitter
}

func (s *runtimeCommittableTranscriptionSession) Commit(ctx context.Context) error {
	if err := s.committer.Commit(ctx); err != nil {
		return classifySessionError(OperationTranscription, s.model.ID.Provider, err)
	}
	return nil
}

func (s *runtimeTranscriptionSession) Metadata() Metadata {
	return cloneMetadata(s.metadata)
}

func (s *runtimeTranscriptionSession) SendAudio(
	ctx context.Context,
	chunk media.AudioChunk,
) error {
	snapshot := chunk.Clone()
	if err := snapshot.Validate(); err != nil {
		return NewError(InvalidRequest, OperationTranscription, "", err)
	}
	if err := s.session.SendAudio(ctx, snapshot); err != nil {
		return classifySessionError(OperationTranscription, s.model.ID.Provider, err)
	}
	return nil
}

func (s *runtimeTranscriptionSession) CloseInput(ctx context.Context) error {
	if err := s.session.CloseInput(ctx); err != nil {
		return classifySessionError(OperationTranscription, s.model.ID.Provider, err)
	}
	return nil
}

func (s *runtimeTranscriptionSession) Next(
	ctx context.Context,
) (TranscriptionEvent, error) {
	event, err := s.session.Next(ctx)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, classifySessionError(
			OperationTranscription,
			s.model.ID.Provider,
			err,
		)
	}
	if isNilValue(event) {
		return nil, NewError(
			InvalidProviderResponse,
			OperationTranscription,
			"",
			errors.New("provider returned a nil transcription event"),
		)
	}
	event = event.Clone()
	var validateErr error
	switch value := event.(type) {
	case PartialTranscriptEvent:
		validateErr = value.ValidateFor(s.config)
	case *PartialTranscriptEvent:
		validateErr = value.ValidateFor(s.config)
	case FinalTranscriptEvent:
		validateErr = value.ValidateFor(s.config)
	case *FinalTranscriptEvent:
		validateErr = value.ValidateFor(s.config)
	default:
		validateErr = event.Validate()
	}
	if validateErr != nil {
		return nil, NewError(
			InvalidProviderResponse,
			OperationTranscription,
			"",
			validateErr,
		)
	}
	return event, nil
}

func (s *runtimeTranscriptionSession) Result() (TranscriptionResponse, error) {
	response, err := s.session.Result()
	if err != nil {
		return TranscriptionResponse{}, classifySessionError(
			OperationTranscription,
			s.model.ID.Provider,
			err,
		)
	}
	response = response.Clone()
	if err := response.validate(&s.config.Language); err != nil {
		return TranscriptionResponse{}, NewError(
			InvalidProviderResponse,
			OperationTranscription,
			"",
			err,
		)
	}
	response.Metadata = cloneMetadata(s.metadata)
	return response, nil
}

func (s *runtimeTranscriptionSession) Close() error {
	s.closeOnce.Do(func() {
		if err := s.session.Close(); err != nil {
			s.closeErr = classifySessionError(
				OperationTranscription,
				s.model.ID.Provider,
				err,
			)
		}
	})
	return s.closeErr
}

type runtimeRealtimeSession struct {
	model    ModelRef
	session  RealtimeSession
	metadata Metadata

	closeOnce sync.Once
	closeErr  error
}

func wrapRealtimeSession(
	model ModelRef,
	session RealtimeSession,
	metadata Metadata,
) OpenedRealtimeSession {
	return &runtimeRealtimeSession{
		model: model, session: session, metadata: cloneMetadata(metadata),
	}
}

func (s *runtimeRealtimeSession) Metadata() Metadata {
	return cloneMetadata(s.metadata)
}

func (s *runtimeRealtimeSession) InputReports() []CompileReport {
	source, ok := s.session.(interface{ inputReports() []CompileReport })
	if !ok {
		return nil
	}
	return source.inputReports()
}

func (s *runtimeRealtimeSession) Send(ctx context.Context, input RealtimeInput) error {
	if isNilValue(input) {
		return NewError(
			InvalidRequest,
			OperationRealtime,
			"",
			errors.New("realtime input is required"),
		)
	}
	snapshot := input.Clone()
	if err := snapshot.Validate(); err != nil {
		return NewError(InvalidRequest, OperationRealtime, "", err)
	}
	if err := s.session.Send(ctx, snapshot); err != nil {
		return classifySessionError(OperationRealtime, s.model.ID.Provider, err)
	}
	return nil
}

func (s *runtimeRealtimeSession) Next(ctx context.Context) (RealtimeEvent, error) {
	event, err := s.session.Next(ctx)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, classifySessionError(
				OperationRealtime,
				s.model.ID.Provider,
				err,
			)
		}
		return nil, classifySessionError(OperationRealtime, s.model.ID.Provider, err)
	}
	if isNilValue(event) {
		return nil, NewError(
			InvalidProviderResponse,
			OperationRealtime,
			"",
			errors.New("provider returned a nil realtime event"),
		)
	}
	event = event.Clone()
	if err := event.Validate(); err != nil {
		return nil, NewError(InvalidProviderResponse, OperationRealtime, "", err)
	}
	return event, nil
}

func (s *runtimeRealtimeSession) CancelResponse(ctx context.Context) error {
	if err := s.session.CancelResponse(ctx); err != nil {
		return classifySessionError(OperationRealtime, s.model.ID.Provider, err)
	}
	return nil
}

func (s *runtimeRealtimeSession) Close() error {
	s.closeOnce.Do(func() {
		if err := s.session.Close(); err != nil {
			s.closeErr = classifySessionError(
				OperationRealtime,
				s.model.ID.Provider,
				err,
			)
		}
	})
	return s.closeErr
}

func cloneMetadata(metadata Metadata) Metadata {
	metadata.Decisions = append([]Decision(nil), metadata.Decisions...)
	return metadata
}

func classifySessionError(
	operation Operation,
	provider string,
	err error,
) error {
	var inferenceErr *Error
	if errors.As(err, &inferenceErr) {
		return err
	}
	return newProviderError(operation, provider, err)
}
