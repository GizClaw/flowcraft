package inferencetest_test

import (
	"context"
	"io"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/inferencetest"
	"github.com/GizClaw/flowcraft/sdk/inference/media"
)

type transcriptionSession struct {
	sends       *inferencetest.Counter
	inputCloses *inferencetest.Counter
	closes      *inferencetest.Counter
}

func (s *transcriptionSession) SendAudio(
	context.Context,
	media.AudioChunk,
) error {
	s.sends.Inc()
	return nil
}
func (s *transcriptionSession) CloseInput(context.Context) error {
	s.inputCloses.Inc()
	return nil
}
func (*transcriptionSession) Next(context.Context) (inference.TranscriptionEvent, error) {
	return nil, io.EOF
}
func (*transcriptionSession) Result() (inference.TranscriptionResponse, error) {
	return inference.TranscriptionResponse{}, nil
}
func (s *transcriptionSession) Close() error {
	s.closes.Inc()
	return nil
}

func TestTranscriptionSessionSuite(t *testing.T) {
	var opens, sends, inputCloses, closes inferencetest.Counter
	driver, err := inference.BindTranscriptionSession(
		func(
			context.Context,
			inference.ModelRef,
			inference.TranscriptionSessionConfig,
		) (inference.Compiled[string], error) {
			return inference.Compiled[string]{
				Wire: "session",
				Report: inference.CompileReport{
					Operation: inference.OperationTranscription,
					Decisions: []inference.Decision{{
						Field:       inference.FieldTranscriptionInputFormat,
						Disposition: inference.Native,
					}},
				},
			}, nil
		},
		func(context.Context, string) (inference.TranscriptionSession, error) {
			opens.Inc()
			return &transcriptionSession{
				sends: &sends, inputCloses: &inputCloses, closes: &closes,
			}, nil
		},
	)
	if err != nil {
		t.Fatalf("BindTranscriptionSession: %v", err)
	}
	model := inference.ModelRef{
		ID: inference.ModelID{Provider: "fake", Name: "audio"},
	}
	runtime := newSessionRuntime(t, inference.Openers{
		Transcription: func(
			context.Context,
			inference.ModelRef,
		) (inference.TranscriptionOperations, error) {
			return inference.TranscriptionOperations{Session: driver}, nil
		},
	})
	inferencetest.RunTranscriptionSession(
		t,
		inferencetest.TranscriptionSessionSuite{
			Runtime: runtime,
			Model:   model,
			Config: func() inference.TranscriptionSessionConfig {
				return inference.TranscriptionSessionConfig{
					InputFormat: media.AudioFormat{
						Encoding:     media.AudioEncodingPCM16,
						SampleRateHz: 16_000,
						Channels:     1,
					},
				}
			},
			Chunk: func() media.AudioChunk {
				return media.AudioChunk{Data: []byte("pcm")}
			},
			SessionOpens:  opens.Load,
			AudioSends:    sends.Load,
			InputCloses:   inputCloses.Load,
			SessionCloses: closes.Load,
		},
	)
}

type realtimeSession struct {
	sends         *inferencetest.Counter
	cancellations *inferencetest.Counter
	closes        *inferencetest.Counter
}

func (s *realtimeSession) Send(context.Context, string) error {
	s.sends.Inc()
	return nil
}
func (*realtimeSession) Next(context.Context) (string, error) {
	return "", io.EOF
}
func (s *realtimeSession) CancelResponse(context.Context) error {
	s.cancellations.Inc()
	return nil
}
func (s *realtimeSession) Close() error {
	s.closes.Inc()
	return nil
}

func TestRealtimeSessionSuite(t *testing.T) {
	var opens, compiles, sends, cancellations, closes inferencetest.Counter
	driver, err := inference.BindRealtime(
		func(
			context.Context,
			inference.ModelRef,
			inference.RealtimeConfig,
		) (inference.Compiled[string], error) {
			return inference.Compiled[string]{
				Wire: "session",
				Report: inference.CompileReport{
					Operation: inference.OperationRealtime,
					Decisions: []inference.Decision{{
						Field:       inference.FieldRealtimeModalities,
						Disposition: inference.Native,
					}},
				},
			}, nil
		},
		func(
			context.Context,
			string,
		) (inference.ProviderRealtimeSession[string, string], error) {
			opens.Inc()
			return &realtimeSession{
				sends: &sends, cancellations: &cancellations, closes: &closes,
			}, nil
		},
		func(
			context.Context,
			inference.ModelRef,
			inference.RealtimeInput,
		) (inference.Compiled[string], error) {
			compiles.Inc()
			return inference.Compiled[string]{
				Wire: "input",
				Report: inference.CompileReport{
					Operation: inference.OperationRealtime,
					Decisions: []inference.Decision{{
						Field:       inference.FieldRealtimeInputText,
						Disposition: inference.Native,
					}},
				},
			}, nil
		},
		func(context.Context, string) (inference.RealtimeEvent, error) {
			return inference.RealtimeTextDeltaEvent{Delta: "ok"}, nil
		},
	)
	if err != nil {
		t.Fatalf("BindRealtime: %v", err)
	}
	model := inference.ModelRef{
		ID: inference.ModelID{Provider: "fake", Name: "audio"},
	}
	runtime := newSessionRuntime(t, inference.Openers{
		Realtime: func(
			context.Context,
			inference.ModelRef,
		) (inference.RealtimeDriver, error) {
			return driver, nil
		},
	})
	inferencetest.RunRealtimeSession(t, inferencetest.RealtimeSessionSuite{
		Runtime: runtime,
		Model:   model,
		Config: func() inference.RealtimeConfig {
			return inference.RealtimeConfig{
				Modalities: []inference.Modality{inference.ModalityText},
			}
		},
		Input: func() inference.RealtimeInput {
			return inference.RealtimeTextInput{Text: "hello"}
		},
		SessionOpens:  opens.Load,
		InputCompiles: compiles.Load,
		InputSends:    sends.Load,
		Cancellations: cancellations.Load,
		SessionCloses: closes.Load,
	})
}

func newSessionRuntime(
	t *testing.T,
	openers inference.Openers,
) *inference.Runtime {
	t.Helper()
	runtime, err := inference.NewRuntime([]inference.ProviderDefinition{{
		ID: "fake",
		Models: []inference.ModelImplementation{{
			Descriptor: inference.ModelDescriptor{
				ID: inference.ModelID{Provider: "fake", Name: "audio"},
			},
			Openers: openers,
		}},
	}})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	return runtime
}
