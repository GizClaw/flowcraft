package inference_test

import (
	"context"
	"io"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// finishRecordingProviderSession is a provider session that records
// FinishInput calls so the wrapper delegation is observable.
type finishRecordingProviderSession struct {
	finishCalls atomic.Int64
}

func (*finishRecordingProviderSession) Send(context.Context, media.AudioChunk) error {
	return nil
}
func (*finishRecordingProviderSession) Next(context.Context) (inference.TranscriptionSessionEvent, error) {
	return inference.TranscriptionSessionEvent{}, io.EOF
}
func (*finishRecordingProviderSession) Interrupt() error { return nil }
func (*finishRecordingProviderSession) Close() error     { return nil }
func (s *finishRecordingProviderSession) FinishInput(context.Context) error {
	s.finishCalls.Add(1)
	return nil
}

// plainProviderSession is a provider session without the finisher
// capability.
type plainProviderSession struct{}

func (*plainProviderSession) Send(context.Context, media.AudioChunk) error {
	return nil
}
func (*plainProviderSession) Next(context.Context) (inference.TranscriptionSessionEvent, error) {
	return inference.TranscriptionSessionEvent{}, io.EOF
}
func (*plainProviderSession) Interrupt() error { return nil }
func (*plainProviderSession) Close() error     { return nil }

func transcriptionSessionDriver[
	RawEvent any,
](
	t *testing.T,
	raw inference.ProviderSession[RawEvent],
) inference.TranscriptionSessionDriver {
	t.Helper()
	compile := inference.Compiler[inference.TranscriptionSessionRequest, string](
		func(
			_ context.Context,
			_ inference.ModelRef,
			request inference.TranscriptionSessionRequest,
		) (inference.Compiled[string], error) {
			return inference.Compiled[string]{
				Wire: "wire",
				Report: inferencetest.NativeReport(
					inference.OperationTranscription,
					request.ActiveFields()...,
				),
			}, nil
		},
	)
	transport := inference.TranscriptionSessionTransport[string, RawEvent](
		func(
			_ context.Context,
			_ string,
		) (inference.ProviderSession[RawEvent], error) {
			return raw, nil
		},
	)
	decode := inference.TranscriptionSessionDecoder[RawEvent](
		func(
			_ context.Context,
			event RawEvent,
		) (inference.TranscriptionSessionEvent, error) {
			return any(event).(inference.TranscriptionSessionEvent), nil
		},
	)
	driver, err := inference.BindTranscribeSession(
		compile,
		transport,
		decode,
	)
	if err != nil {
		t.Fatalf("BindTranscribeSession: %v", err)
	}
	return driver
}

func openTranscriptionSession(
	t *testing.T,
	driver inference.TranscriptionSessionDriver,
) inference.TranscriptionSession {
	t.Helper()
	session, err := driver.Open(
		context.Background(),
		inferencetest.DefaultFakeTranscribeModel,
		inference.TranscriptionSessionRequest{
			InputFormat: media.AudioFormat{
				Encoding:     media.AudioEncodingPCM16,
				SampleRateHz: 16000,
				Channels:     1,
			},
		},
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return session
}

func TestTranscriptionSessionFinishInputDelegates(t *testing.T) {
	raw := &finishRecordingProviderSession{}
	driver := transcriptionSessionDriver(
		t,
		raw,
	)
	session := openTranscriptionSession(t, driver)

	finisher, ok := session.(inference.TranscriptionSessionFinisher)
	if !ok {
		t.Fatalf("session %T does not expose TranscriptionSessionFinisher", session)
	}
	if err := finisher.FinishInput(context.Background()); err != nil {
		t.Fatalf("FinishInput: %v", err)
	}
	if calls := raw.finishCalls.Load(); calls != 1 {
		t.Fatalf("finish calls = %d, want 1", calls)
	}

	for {
		if _, err := session.Next(context.Background()); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
	if _, err := session.Result(); err != nil {
		t.Fatalf("Result: %v", err)
	}
}

func TestTranscriptionSessionFinishInputNoopWhenUnsupported(t *testing.T) {
	driver := transcriptionSessionDriver(
		t,
		&plainProviderSession{},
	)
	session := openTranscriptionSession(t, driver)

	finisher, ok := session.(inference.TranscriptionSessionFinisher)
	if !ok {
		t.Fatalf("session %T does not expose TranscriptionSessionFinisher", session)
	}
	if err := finisher.FinishInput(context.Background()); err != nil {
		t.Fatalf("FinishInput on unsupported session: %v", err)
	}
}
