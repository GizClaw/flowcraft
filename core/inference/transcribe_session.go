package inference

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/message/media"
)

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
