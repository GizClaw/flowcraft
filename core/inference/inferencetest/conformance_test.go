package inferencetest_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/message"
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
