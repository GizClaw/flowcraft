package inference

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/message/media"
)

// This file extends the provider contract matrix to opened sessions: which
// session method failure becomes which ErrorKind, which errors pass through
// untouched, and how session lifecycle (Close once, metadata ownership)
// behaves at the framework boundary.

type stubTranscriptionSession struct {
	sendErr       error
	closeInputErr error
	commitErr     error
	result        TranscriptionResponse
	resultErr     error
	closeErr      error

	events   []TranscriptionEvent
	eventErr error
	index    int

	sends  int
	closes int
}

func (s *stubTranscriptionSession) SendAudio(
	context.Context,
	media.AudioChunk,
) error {
	s.sends++
	return s.sendErr
}

func (s *stubTranscriptionSession) CloseInput(context.Context) error {
	return s.closeInputErr
}

func (s *stubTranscriptionSession) Commit(context.Context) error {
	return s.commitErr
}

func (s *stubTranscriptionSession) Next(context.Context) (TranscriptionEvent, error) {
	if s.eventErr != nil {
		return nil, s.eventErr
	}
	if s.index == len(s.events) {
		return nil, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (s *stubTranscriptionSession) Result() (TranscriptionResponse, error) {
	return s.result, s.resultErr
}

func (s *stubTranscriptionSession) Close() error {
	s.closes++
	return s.closeErr
}

func sessionTestModel() ModelRef {
	return ModelRef{ID: ModelID{Provider: "fake", Name: "transcribe"}}
}

func sessionTestMetadata() Metadata {
	return Metadata{
		Model:     sessionTestModel().ID,
		Operation: OperationTranscription,
		Decisions: []Decision{{Field: FieldTranscriptionAudio, Disposition: Native}},
	}
}

func TestContractMatrixTranscriptionSessionMethodErrors(t *testing.T) {
	tests := []struct {
		name string
		call func(OpenedTranscriptionSession) error
	}{
		{"send audio", func(s OpenedTranscriptionSession) error {
			return s.SendAudio(context.Background(), media.AudioChunk{Data: []byte{1}})
		}},
		{"close input", func(s OpenedTranscriptionSession) error {
			return s.CloseInput(context.Background())
		}},
		{"result", func(s OpenedTranscriptionSession) error {
			_, err := s.Result()
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failure := errors.New("transport lost")
			stub := &stubTranscriptionSession{
				sendErr:       failure,
				closeInputErr: failure,
				resultErr:     failure,
			}
			session := wrapTranscriptionSession(
				sessionTestModel(),
				TranscriptionSessionConfig{},
				stub,
				sessionTestMetadata(),
			)
			err := tt.call(session)
			if !IsKind(err, ProviderFailure) || !errdefs.IsNotAvailable(err) {
				t.Fatalf("error = %v, want not-available ProviderFailure", err)
			}
		})
	}
}

func TestContractMatrixTranscriptionSessionBoundaries(t *testing.T) {
	t.Run("invalid audio chunk is rejected before provider IO", func(t *testing.T) {
		stub := &stubTranscriptionSession{}
		session := wrapTranscriptionSession(
			sessionTestModel(),
			TranscriptionSessionConfig{},
			stub,
			sessionTestMetadata(),
		)
		err := session.SendAudio(context.Background(), media.AudioChunk{})
		if !IsKind(err, InvalidRequest) || !errdefs.IsValidation(err) {
			t.Fatalf("error = %v, want validation InvalidRequest", err)
		}
		if stub.sends != 0 {
			t.Fatal("invalid chunk reached the provider session")
		}
	})

	t.Run("io EOF passes through unwrapped", func(t *testing.T) {
		session := wrapTranscriptionSession(
			sessionTestModel(),
			TranscriptionSessionConfig{},
			&stubTranscriptionSession{},
			sessionTestMetadata(),
		)
		_, err := session.Next(context.Background())
		if err != io.EOF {
			t.Fatalf("Next error = %v, want exact io.EOF", err)
		}
	})

	t.Run("nil event is an invalid provider response", func(t *testing.T) {
		session := wrapTranscriptionSession(
			sessionTestModel(),
			TranscriptionSessionConfig{},
			&stubTranscriptionSession{events: []TranscriptionEvent{nil}},
			sessionTestMetadata(),
		)
		_, err := session.Next(context.Background())
		if !IsKind(err, InvalidProviderResponse) || !errdefs.IsInternal(err) {
			t.Fatalf("Next error = %v, want internal InvalidProviderResponse", err)
		}
	})

	t.Run("invalid event is an invalid provider response", func(t *testing.T) {
		session := wrapTranscriptionSession(
			sessionTestModel(),
			TranscriptionSessionConfig{},
			&stubTranscriptionSession{events: []TranscriptionEvent{
				PartialTranscriptEvent{},
			}},
			sessionTestMetadata(),
		)
		_, err := session.Next(context.Background())
		if !IsKind(err, InvalidProviderResponse) {
			t.Fatalf("Next error = %v, want InvalidProviderResponse", err)
		}
	})

	t.Run("invalid result is an invalid provider response", func(t *testing.T) {
		session := wrapTranscriptionSession(
			sessionTestModel(),
			TranscriptionSessionConfig{},
			&stubTranscriptionSession{result: TranscriptionResponse{
				Text:     "hello",
				Language: &TranscriptLanguage{},
			}},
			sessionTestMetadata(),
		)
		_, err := session.Result()
		if !IsKind(err, InvalidProviderResponse) {
			t.Fatalf("Result error = %v, want InvalidProviderResponse", err)
		}
	})

	t.Run("result receives the opening metadata", func(t *testing.T) {
		session := wrapTranscriptionSession(
			sessionTestModel(),
			TranscriptionSessionConfig{},
			&stubTranscriptionSession{
				result: TranscriptionResponse{Text: "hello"},
			},
			sessionTestMetadata(),
		)
		response, err := session.Result()
		if err != nil {
			t.Fatalf("Result: %v", err)
		}
		if response.Metadata.Model != sessionTestModel().ID ||
			response.Metadata.Operation != OperationTranscription {
			t.Fatalf("result metadata = %+v", response.Metadata)
		}
	})

	t.Run("close runs once and classifies failure", func(t *testing.T) {
		failure := errors.New("hangup failed")
		stub := &stubTranscriptionSession{closeErr: failure}
		session := wrapTranscriptionSession(
			sessionTestModel(),
			TranscriptionSessionConfig{},
			stub,
			sessionTestMetadata(),
		)
		first := session.Close()
		second := session.Close()
		if stub.closes != 1 {
			t.Fatalf("provider Close called %d times, want 1", stub.closes)
		}
		if !IsKind(first, ProviderFailure) || !errors.Is(second, first) && second != first {
			t.Fatalf("Close errors = %v/%v", first, second)
		}
	})

	t.Run("commit failure is a provider failure", func(t *testing.T) {
		stub := &stubTranscriptionSession{commitErr: errors.New("commit lost")}
		session := wrapTranscriptionSession(
			sessionTestModel(),
			TranscriptionSessionConfig{},
			stub,
			sessionTestMetadata(),
		)
		committer, ok := session.(TranscriptionCommitter)
		if !ok {
			t.Fatal("wrapped session lost the committer capability")
		}
		if err := committer.Commit(context.Background()); !IsKind(err, ProviderFailure) {
			t.Fatalf("Commit error = %v, want ProviderFailure", err)
		}
	})

	t.Run("inference errors pass through without reclassification", func(t *testing.T) {
		structured := NewError(
			OperationInterrupted,
			OperationTranscription,
			"",
			context.Canceled,
		)
		session := wrapTranscriptionSession(
			sessionTestModel(),
			TranscriptionSessionConfig{},
			&stubTranscriptionSession{sendErr: structured},
			sessionTestMetadata(),
		)
		err := session.SendAudio(
			context.Background(),
			media.AudioChunk{Data: []byte{1}},
		)
		if err != structured {
			t.Fatalf("error = %v, want passthrough of %v", err, structured)
		}
	})
}

type stubRealtimeSession struct {
	sendErr       error
	cancelErr     error
	closeErr      error
	events        []RealtimeEvent
	eventErr      error
	index         int
	sends         int
	closes        int
	cancellations int
}

func (s *stubRealtimeSession) Send(context.Context, RealtimeInput) error {
	s.sends++
	return s.sendErr
}

func (s *stubRealtimeSession) Next(context.Context) (RealtimeEvent, error) {
	if s.eventErr != nil {
		return nil, s.eventErr
	}
	if s.index == len(s.events) {
		return nil, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (s *stubRealtimeSession) CancelResponse(context.Context) error {
	s.cancellations++
	return s.cancelErr
}

func (s *stubRealtimeSession) Close() error {
	s.closes++
	return s.closeErr
}

func TestContractMatrixRealtimeSessionBoundaries(t *testing.T) {
	model := ModelRef{ID: ModelID{Provider: "fake", Name: "live"}}
	metadata := Metadata{
		Model:     model.ID,
		Operation: OperationRealtime,
		Decisions: []Decision{{Field: FieldRealtimeModalities, Disposition: Native}},
	}
	wrap := func(stub *stubRealtimeSession) OpenedRealtimeSession {
		return wrapRealtimeSession(model, stub, metadata)
	}

	t.Run("invalid input is rejected before provider IO", func(t *testing.T) {
		stub := &stubRealtimeSession{}
		err := wrap(stub).Send(context.Background(), nil)
		if !IsKind(err, InvalidRequest) {
			t.Fatalf("Send error = %v, want InvalidRequest", err)
		}
		if stub.sends != 0 {
			t.Fatal("invalid input reached the provider session")
		}
	})

	t.Run("send failure is a provider failure", func(t *testing.T) {
		stub := &stubRealtimeSession{sendErr: errors.New("write failed")}
		err := wrap(stub).Send(
			context.Background(),
			RealtimeTextInput{Text: "hello"},
		)
		if !IsKind(err, ProviderFailure) || !errdefs.IsNotAvailable(err) {
			t.Fatalf("Send error = %v, want not-available ProviderFailure", err)
		}
	})

	t.Run("io EOF passes through unwrapped", func(t *testing.T) {
		_, err := wrap(&stubRealtimeSession{}).Next(context.Background())
		if err != io.EOF {
			t.Fatalf("Next error = %v, want exact io.EOF", err)
		}
	})

	t.Run("nil and invalid events are invalid provider responses", func(t *testing.T) {
		_, err := wrap(&stubRealtimeSession{
			events: []RealtimeEvent{nil},
		}).Next(context.Background())
		if !IsKind(err, InvalidProviderResponse) {
			t.Fatalf("nil event error = %v", err)
		}
		_, err = wrap(&stubRealtimeSession{
			events: []RealtimeEvent{RealtimeTextDeltaEvent{}},
		}).Next(context.Background())
		if !IsKind(err, InvalidProviderResponse) {
			t.Fatalf("invalid event error = %v", err)
		}
	})

	t.Run("cancel and close failures are provider failures", func(t *testing.T) {
		failure := errors.New("lost")
		stub := &stubRealtimeSession{cancelErr: failure, closeErr: failure}
		session := wrap(stub)
		if err := session.CancelResponse(context.Background()); !IsKind(err, ProviderFailure) {
			t.Fatalf("CancelResponse error = %v", err)
		}
		if err := session.Close(); !IsKind(err, ProviderFailure) {
			t.Fatalf("Close error = %v", err)
		}
		if err := session.Close(); err == nil || stub.closes != 1 {
			t.Fatalf("second Close = %v, closes = %d", err, stub.closes)
		}
	})
}

func TestContractMatrixSessionMetadataIsCallerOwned(t *testing.T) {
	metadata := sessionTestMetadata()
	session := wrapTranscriptionSession(
		sessionTestModel(),
		TranscriptionSessionConfig{},
		&stubTranscriptionSession{},
		metadata,
	)
	metadata.Decisions[0].Disposition = Rejected
	got := session.Metadata()
	if got.Decisions[0].Disposition != Native {
		t.Fatal("session retained caller-owned metadata")
	}
	got.Decisions[0].Disposition = Rejected
	if session.Metadata().Decisions[0].Disposition != Native {
		t.Fatal("Metadata() shared its decisions slice with the caller")
	}
}
