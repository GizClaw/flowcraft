package inferencetest_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/resource"
)

func TestEmbedFakeAssembly(t *testing.T) {
	fake := &inferencetest.EmbedFake{}
	assembly := fake.Assembly(t)

	request := inference.EmbedRequest{
		Items: []inference.EmbedItem{{
			Content: message.Content{Parts: []message.Part{message.TextPart{Text: "hi"}}},
		}},
	}
	response, err := assembly.Embed(context.Background(), inferencetest.DefaultFakeEmbedModel, request)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(response.Embeddings) != 1 {
		t.Fatalf("embeddings = %d, want 1", len(response.Embeddings))
	}
	if len(fake.Requests()) != 1 {
		t.Fatalf("compiler requests = %d, want 1", len(fake.Requests()))
	}
}

func TestRunEmbedUnary(t *testing.T) {
	calls := &inferencetest.Counter{}
	driver, err := inference.BindEmbed(
		inference.Compiler[inference.EmbedRequest, string](
			func(_ context.Context, _ inference.ModelRef, request inference.EmbedRequest) (inference.Compiled[string], error) {
				return inference.Compiled[string]{
					Wire: "wire",
					Report: inferencetest.NativeReport(
						inference.OperationEmbed,
						request.ActiveFields()...,
					),
				}, nil
			},
		),
		countingTransport(calls, func(_ context.Context, wire string) (string, error) { return wire, nil }),
		inference.Decoder[string, inference.EmbedResponse](
			func(_ context.Context, _ string) (inference.EmbedResponse, error) {
				return inference.EmbedResponse{
					Embeddings: []inference.Embedding{{Vector: []float32{1}}},
				}, nil
			},
		),
	)
	if err != nil {
		t.Fatalf("BindEmbed: %v", err)
	}
	inferencetest.RunEmbedUnary(t, inferencetest.EmbedUnarySuite{
		Model: inferencetest.DefaultFakeEmbedModel,
		Request: func() inference.EmbedRequest {
			return inference.EmbedRequest{
				Items: []inference.EmbedItem{{
					Content: message.Content{Parts: []message.Part{message.TextPart{Text: "hi"}}},
				}},
			}
		},
		Driver:         driver,
		TransportCalls: calls.Load,
	})
}

func TestRunGenerateStreamFailure(t *testing.T) {
	calls := &inferencetest.Counter{}
	driver, err := inference.BindGenerateStream(
		inference.GenerateCompiler[string](
			func(_ context.Context, _ inference.ModelRef, request inference.GenerateRequest, shape inference.GenerateExecutionShape) (inference.Compiled[string], error) {
				return inference.Compiled[string]{
					Wire: "wire",
					Report: inferencetest.NativeReport(
						inference.OperationGenerate,
						request.ActiveFieldsFor(shape)...,
					),
				}, nil
			},
		),
		countingTransport(calls, func(_ context.Context, _ string) (inference.ProviderStream[inference.GenerateStreamEvent], error) {
			return &failingGenerateStream{}, nil
		}),
		inference.GenerateStreamDecoder[inference.GenerateStreamEvent](
			func(_ context.Context, event inference.GenerateStreamEvent) (inference.GenerateStreamEvent, error) {
				return event, nil
			},
		),
	)
	if err != nil {
		t.Fatalf("BindGenerateStream: %v", err)
	}
	inferencetest.RunGenerateStreamFailure(t, inferencetest.GenerateStreamFailureSuite{
		Model: inferencetest.DefaultFakeModel,
		Request: func() inference.GenerateRequest {
			return inference.GenerateRequest{Input: inference.GenerateInput{
				Role: inference.InputRoleUser,
				Content: inference.InputContent{
					Content: message.Content{Parts: []message.Part{message.TextPart{Text: "hi"}}},
					Intent:  inference.Intent{Text: &inference.TextIntent{}},
				},
			}}
		},
		Driver:         driver,
		TransportCalls: calls.Load,
		AssertError: func(t *testing.T, err error) {
			if !errors.Is(err, errStreamBoom) {
				t.Fatalf("Next error = %v, want errStreamBoom", err)
			}
		},
	})
}

func TestRunGenerateConcurrent(t *testing.T) {
	calls := &inferencetest.Counter{}
	operations, err := inference.BindGenerateOperations(
		inference.GenerateCompiler[string](
			func(_ context.Context, _ inference.ModelRef, request inference.GenerateRequest, shape inference.GenerateExecutionShape) (inference.Compiled[string], error) {
				return inference.Compiled[string]{
					Wire: "wire",
					Report: inferencetest.NativeReport(
						inference.OperationGenerate,
						request.ActiveFieldsFor(shape)...,
					),
				}, nil
			},
		),
		countingTransport(calls, func(_ context.Context, wire string) (string, error) { return wire, nil }),
		inference.Decoder[string, inference.GenerateResponse](
			func(_ context.Context, _ string) (inference.GenerateResponse, error) {
				return inference.GenerateResponse{
					Message: message.Message{
						Role:    message.RoleAssistant,
						Content: message.Content{Parts: []message.Part{message.TextPart{Text: "ok"}}},
					},
					FinishReason: inference.FinishCompleted,
				}, nil
			},
		),
		countingTransport(calls, func(_ context.Context, _ string) (inference.ProviderStream[inference.GenerateStreamEvent], error) {
			return &okGenerateStream{events: []inference.GenerateStreamEvent{
				{PartIndex: 0, Delta: inference.TextPartDelta{Text: "ok"}},
				{FinishReason: inference.FinishCompleted},
			}}, nil
		}),
		inference.GenerateStreamDecoder[inference.GenerateStreamEvent](
			func(_ context.Context, event inference.GenerateStreamEvent) (inference.GenerateStreamEvent, error) {
				return event, nil
			},
		),
	)
	if err != nil {
		t.Fatalf("BindGenerateOperations: %v", err)
	}
	inferencetest.RunGenerateConcurrent(t, inferencetest.GenerateConcurrentSuite{
		Model: inferencetest.DefaultFakeModel,
		Request: func() inference.GenerateRequest {
			return inference.GenerateRequest{Input: inference.GenerateInput{
				Role: inference.InputRoleUser,
				Content: inference.InputContent{
					Content: message.Content{Parts: []message.Part{message.TextPart{Text: "hi"}}},
					Intent:  inference.Intent{Text: &inference.TextIntent{}},
				},
			}}
		},
		Unary:  operations.Unary,
		Stream: operations.Stream,
	})
}

var errStreamBoom = errors.New("stream boom")

type failingGenerateStream struct{}

func (*failingGenerateStream) Next(context.Context) (inference.GenerateStreamEvent, error) {
	return inference.GenerateStreamEvent{}, errStreamBoom
}
func (*failingGenerateStream) Close() error { return nil }

type okGenerateStream struct {
	events []inference.GenerateStreamEvent
	index  int
}

func (s *okGenerateStream) Next(context.Context) (inference.GenerateStreamEvent, error) {
	if s.index >= len(s.events) {
		return inference.GenerateStreamEvent{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}
func (*okGenerateStream) Close() error { return nil }

func countingTransport[Wire, Raw any](
	calls *inferencetest.Counter,
	next inference.Transport[Wire, Raw],
) inference.Transport[Wire, Raw] {
	return func(ctx context.Context, wire Wire) (Raw, error) {
		calls.Inc()
		return next(ctx, wire)
	}
}

func TestTranscribeFakeAssembly(t *testing.T) {
	fake := &inferencetest.TranscriptionFake{}
	assembly := fake.Assembly(t)

	request := inference.TranscriptionRequest{
		Audio: mustAudioSource(t),
	}
	response, err := assembly.Transcribe(
		context.Background(),
		inferencetest.DefaultFakeTranscribeModel,
		request,
	)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if response.Text != "ok" {
		t.Fatalf("Text = %q, want %q", response.Text, "ok")
	}
	if len(fake.Requests()) != 1 {
		t.Fatalf("compiler requests = %d, want 1", len(fake.Requests()))
	}
}

func TestRunTranscribeUnary(t *testing.T) {
	calls := &inferencetest.Counter{}
	driver, err := inference.BindTranscribe(
		inference.Compiler[inference.TranscriptionRequest, string](
			func(_ context.Context, _ inference.ModelRef, request inference.TranscriptionRequest) (inference.Compiled[string], error) {
				return inference.Compiled[string]{
					Wire: "wire",
					Report: inferencetest.NativeReport(
						inference.OperationTranscription,
						request.ActiveFields()...,
					),
				}, nil
			},
		),
		countingTransport(calls, func(_ context.Context, wire string) (string, error) {
			return wire, nil
		}),
		inference.Decoder[string, inference.TranscriptionResponse](
			func(_ context.Context, _ string) (inference.TranscriptionResponse, error) {
				return inference.TranscriptionResponse{
					Text: "hello",
					Segments: []inference.TranscriptionSegment{{
						Text: "hello",
					}},
				}, nil
			},
		),
	)
	if err != nil {
		t.Fatalf("BindTranscribe: %v", err)
	}
	inferencetest.RunTranscribeUnary(t, inferencetest.TranscriptionUnarySuite{
		Model: inferencetest.DefaultFakeTranscribeModel,
		Request: func() inference.TranscriptionRequest {
			return inference.TranscriptionRequest{Audio: mustAudioSource(t)}
		},
		Driver:         driver,
		TransportCalls: calls.Load,
	})
}

type scriptedRawEvent struct {
	Text  string
	Final bool
}

type scriptedRawSession struct {
	mu          sync.Mutex
	events      []scriptedRawEvent
	sends       int
	interrupted bool
	closed      bool
}

func (s *scriptedRawSession) Send(
	context.Context,
	media.AudioChunk,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sends++
	return nil
}

func (s *scriptedRawSession) Next(
	context.Context,
) (scriptedRawEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.interrupted {
		return scriptedRawEvent{}, errors.New("session interrupted")
	}
	if len(s.events) == 0 {
		return scriptedRawEvent{}, io.EOF
	}
	event := s.events[0]
	s.events = s.events[1:]
	return event, nil
}

func (s *scriptedRawSession) Interrupt() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.interrupted = true
	return nil
}

func (s *scriptedRawSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func countingTranscribeSessionTransport[Wire, RawEvent any](
	calls *inferencetest.Counter,
	next inference.TranscriptionSessionTransport[Wire, RawEvent],
) inference.TranscriptionSessionTransport[Wire, RawEvent] {
	return func(
		ctx context.Context,
		wire Wire,
	) (inference.ProviderSession[RawEvent], error) {
		calls.Inc()
		return next(ctx, wire)
	}
}

func bindScriptedSession(
	calls *inferencetest.Counter,
	raw *scriptedRawSession,
) (inference.TranscriptionSessionDriver, error) {
	return inference.BindTranscribeSession(
		inference.Compiler[inference.TranscriptionSessionRequest, string](
			func(_ context.Context, _ inference.ModelRef, request inference.TranscriptionSessionRequest) (inference.Compiled[string], error) {
				return inference.Compiled[string]{
					Wire: "wire",
					Report: inferencetest.NativeReport(
						inference.OperationTranscription,
						request.ActiveFields()...,
					),
				}, nil
			},
		),
		countingTranscribeSessionTransport(
			calls,
			func(_ context.Context, _ string) (inference.ProviderSession[scriptedRawEvent], error) {
				return raw, nil
			},
		),
		inference.TranscriptionSessionDecoder[scriptedRawEvent](
			func(_ context.Context, event scriptedRawEvent) (inference.TranscriptionSessionEvent, error) {
				return inference.TranscriptionSessionEvent{
					Text:  event.Text,
					Final: event.Final,
				}, nil
			},
		),
	)
}

func sessionRequest() inference.TranscriptionSessionRequest {
	return inference.TranscriptionSessionRequest{
		InputFormat: media.AudioFormat{
			Encoding:     media.AudioEncodingPCM16,
			SampleRateHz: 16000,
			Channels:     1,
		},
	}
}

func mustAudioSource(t *testing.T) media.AudioSource {
	t.Helper()
	source, err := media.NewAudioBytes([]byte{1, 2, 3, 4}, "audio/wav")
	if err != nil {
		t.Fatalf("NewAudioBytes: %v", err)
	}
	return source
}

func TestRunTranscribeSession(t *testing.T) {
	calls := &inferencetest.Counter{}
	raw := &scriptedRawSession{events: []scriptedRawEvent{
		{Text: "hel"},
		{Text: "hello", Final: true},
		{Text: "wor"},
		{Text: "world", Final: true},
	}}
	driver, err := bindScriptedSession(calls, raw)
	if err != nil {
		t.Fatalf("BindTranscribeSession: %v", err)
	}
	inferencetest.RunTranscribeSession(t, inferencetest.TranscriptionSessionSuite{
		Model:   inferencetest.DefaultFakeTranscribeModel,
		Request: sessionRequest,
		Driver:  driver,
		Feed: func(ctx context.Context, session inference.TranscriptionSession) error {
			for i := 0; i < 3; i++ {
				if err := session.Send(ctx, media.AudioChunk{Data: []byte{0, 0}}); err != nil {
					return err
				}
			}
			return nil
		},
		TransportCalls: calls.Load,
		AssertResult: func(t *testing.T, response inference.TranscriptionResponse) {
			if response.Text != "hello\nworld" {
				t.Fatalf("Text = %q, want %q", response.Text, "hello\nworld")
			}
			if len(response.Segments) != 2 ||
				response.Segments[0].Text != "hello" ||
				response.Segments[1].Text != "world" {
				t.Fatalf("Segments = %+v", response.Segments)
			}
		},
	})
	if raw.sends != 3 {
		t.Fatalf("provider sends = %d, want 3", raw.sends)
	}
}

func TestTranscribeSessionInterrupt(t *testing.T) {
	calls := &inferencetest.Counter{}
	raw := &scriptedRawSession{events: []scriptedRawEvent{{Text: "partial"}}}
	driver, err := bindScriptedSession(calls, raw)
	if err != nil {
		t.Fatalf("BindTranscribeSession: %v", err)
	}
	session, err := driver.Open(
		context.Background(),
		inferencetest.DefaultFakeTranscribeModel,
		sessionRequest(),
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := session.Send(
		context.Background(),
		media.AudioChunk{Data: []byte{0, 0}},
	); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := session.Interrupt(); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if _, err := session.Next(context.Background()); !isInterrupted(err) {
		t.Fatalf("Next after Interrupt = %v, want interruption", err)
	}
	if _, err := session.Result(); !isInterrupted(err) {
		t.Fatalf("Result after Interrupt = %v, want interruption", err)
	}
	if err := session.Send(
		context.Background(),
		media.AudioChunk{Data: []byte{0, 0}},
	); !isInterrupted(err) {
		t.Fatalf("Send after Interrupt = %v, want interruption", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func isInterrupted(err error) bool {
	if err == nil {
		return false
	}
	var inferenceErr *inference.Error
	return errors.As(err, &inferenceErr) &&
		inferenceErr.Kind == inference.OperationInterrupted
}

func TestRunTranscribeDualOperations(t *testing.T) {
	calls := &inferencetest.Counter{}
	raw := &scriptedRawSession{events: []scriptedRawEvent{{Text: "hi", Final: true}}}
	operations, err := inference.BindTranscribeOperations(
		inference.Compiler[inference.TranscriptionRequest, string](
			func(_ context.Context, _ inference.ModelRef, request inference.TranscriptionRequest) (inference.Compiled[string], error) {
				return inference.Compiled[string]{
					Wire: "wire",
					Report: inferencetest.NativeReport(
						inference.OperationTranscription,
						request.ActiveFields()...,
					),
				}, nil
			},
		),
		countingTransport(calls, func(_ context.Context, wire string) (string, error) {
			return wire, nil
		}),
		inference.Decoder[string, inference.TranscriptionResponse](
			func(_ context.Context, _ string) (inference.TranscriptionResponse, error) {
				return inference.TranscriptionResponse{
					Text:     "hi",
					Segments: []inference.TranscriptionSegment{{Text: "hi"}},
				}, nil
			},
		),
		inference.Compiler[inference.TranscriptionSessionRequest, string](
			func(_ context.Context, _ inference.ModelRef, request inference.TranscriptionSessionRequest) (inference.Compiled[string], error) {
				return inference.Compiled[string]{
					Wire: "wire",
					Report: inferencetest.NativeReport(
						inference.OperationTranscription,
						request.ActiveFields()...,
					),
				}, nil
			},
		),
		countingTranscribeSessionTransport(
			calls,
			func(_ context.Context, _ string) (inference.ProviderSession[scriptedRawEvent], error) {
				return raw, nil
			},
		),
		inference.TranscriptionSessionDecoder[scriptedRawEvent](
			func(_ context.Context, event scriptedRawEvent) (inference.TranscriptionSessionEvent, error) {
				return inference.TranscriptionSessionEvent{
					Text:  event.Text,
					Final: event.Final,
				}, nil
			},
		),
	)
	if err != nil {
		t.Fatalf("BindTranscribeOperations: %v", err)
	}
	if err := operations.Validate(); err != nil {
		t.Fatalf("TranscribeOperations.Validate: %v", err)
	}

	model := inferencetest.DefaultFakeTranscribeModel
	definition := inference.ProviderDefinition{
		ID: model.ID.Provider,
		Profiles: []inference.ProfileDefinition{{
			ID:         model.Profile,
			Operations: []inference.Operation{inference.OperationTranscription},
		}},
		Models: []inference.ModelImplementation{{
			Descriptor: inference.ModelDescriptor{ID: model.ID},
			Openers: inference.Openers{
				Transcribe: func(
					_ context.Context,
					_ inference.ModelRef,
				) (inference.TranscribeOperations, error) {
					return operations, nil
				},
			},
		}},
	}
	value, err := inference.Factory{}.New(context.Background(), resource.Input{
		Deps: map[string]any{"provider." + model.ID.Provider: definition},
	})
	if err != nil {
		t.Fatalf("build assembly: %v", err)
	}
	assembly := value.(*inference.Assembly)

	response, err := assembly.Transcribe(
		context.Background(),
		model,
		inference.TranscriptionRequest{Audio: mustAudioSource(t)},
	)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if response.Text != "hi" {
		t.Fatalf("unary Text = %q, want %q", response.Text, "hi")
	}
	session, err := assembly.TranscribeSession(
		context.Background(),
		model,
		sessionRequest(),
	)
	if err != nil {
		t.Fatalf("TranscribeSession: %v", err)
	}
	if err := session.Send(
		context.Background(),
		media.AudioChunk{Data: []byte{0, 0}},
	); err != nil {
		t.Fatalf("session Send: %v", err)
	}
	for {
		_, err := session.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("session Next: %v", err)
		}
	}
	sessionResult, err := session.Result()
	if err != nil {
		t.Fatalf("session Result: %v", err)
	}
	if sessionResult.Text != "hi" {
		t.Fatalf("session Text = %q, want %q", sessionResult.Text, "hi")
	}
}
