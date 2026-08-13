package qwen

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/message"
)

func conformanceTextRequest() inference.GenerateRequest {
	return inference.GenerateRequest{Input: inference.GenerateInput{
		Role: inference.InputRoleUser,
		Content: inference.InputContent{
			Content: message.Content{Parts: []message.Part{message.TextPart{Text: "hi"}}},
			Intent:  inference.Intent{Text: &inference.TextIntent{}},
		},
	}}
}

func conformanceModel(name string) inference.ModelRef {
	return inference.ModelRef{
		ID:      inference.ModelID{Provider: driverID, Name: name},
		Profile: "default",
	}
}

func conformanceEmbedRequest() inference.EmbedRequest {
	return inference.EmbedRequest{Items: []inference.EmbedItem{{
		Content: message.Content{Parts: []message.Part{message.TextPart{Text: "hi"}}},
	}}}
}

func conformanceGenerateDrivers[Wire any](
	t *testing.T,
	compile inference.GenerateCompiler[Wire],
	calls *inferencetest.Counter,
) (inference.GenerateDriver, inference.GenerateStreamDriver) {
	t.Helper()
	operations, err := inference.BindGenerateOperations(
		compile,
		countingTransport(calls, func(_ context.Context, wire Wire) (Wire, error) { return wire, nil }),
		inference.Decoder[Wire, inference.GenerateResponse](
			func(_ context.Context, _ Wire) (inference.GenerateResponse, error) {
				return inference.GenerateResponse{
					Message: message.Message{
						Role:    message.RoleAssistant,
						Content: message.Content{Parts: []message.Part{message.TextPart{Text: "ok"}}},
					},
					FinishReason: inference.FinishCompleted,
				}, nil
			},
		),
		countingTransport(calls, func(_ context.Context, _ Wire) (inference.ProviderStream[inference.GenerateStreamEvent], error) {
			return &conformanceStream{events: []inference.GenerateStreamEvent{
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
	return operations.Unary, operations.Stream
}

func conformanceEmbedDriver[Wire any](
	t *testing.T,
	compile inference.Compiler[inference.EmbedRequest, Wire],
	calls *inferencetest.Counter,
) inference.EmbedDriver {
	t.Helper()
	driver, err := inference.BindEmbed(
		compile,
		countingTransport(calls, func(_ context.Context, wire Wire) (Wire, error) { return wire, nil }),
		inference.Decoder[Wire, inference.EmbedResponse](
			func(_ context.Context, _ Wire) (inference.EmbedResponse, error) {
				return inference.EmbedResponse{
					Embeddings: []inference.Embedding{{Vector: []float32{1}}},
				}, nil
			},
		),
	)
	if err != nil {
		t.Fatalf("BindEmbed: %v", err)
	}
	return driver
}

func countingTransport[Wire, Raw any](
	calls *inferencetest.Counter,
	next inference.Transport[Wire, Raw],
) inference.Transport[Wire, Raw] {
	return func(ctx context.Context, wire Wire) (Raw, error) {
		calls.Inc()
		return next(ctx, wire)
	}
}

type conformanceStream struct {
	events []inference.GenerateStreamEvent
	index  int
}

func (s *conformanceStream) Next(context.Context) (inference.GenerateStreamEvent, error) {
	if s.index >= len(s.events) {
		return inference.GenerateStreamEvent{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}
func (*conformanceStream) Close() error { return nil }

func TestConformanceGenerateConcurrent(t *testing.T) {
	calls := &inferencetest.Counter{}
	model := "qwen-plus"
	unary, stream := conformanceGenerateDrivers(t, compileGenerate(model, catalog[model]), calls)
	inferencetest.RunGenerateConcurrent(t, inferencetest.GenerateConcurrentSuite{
		Model:      conformanceModel(model),
		Request:    conformanceTextRequest,
		Unary:      unary,
		Stream:     stream,
		Goroutines: 2,
	})
}

func TestConformanceEmbedUnary(t *testing.T) {
	calls := &inferencetest.Counter{}
	model := "text-embedding-v4"
	driver := conformanceEmbedDriver(t, compileEmbed(model, catalog[model]), calls)
	inferencetest.RunEmbedUnary(t, inferencetest.EmbedUnarySuite{
		Model:          conformanceModel(model),
		Request:        conformanceEmbedRequest,
		Driver:         driver,
		TransportCalls: calls.Load,
	})
}

var errStreamFailure = errors.New("stream failure")

type failingStream struct{}

func (*failingStream) Next(context.Context) (inference.GenerateStreamEvent, error) {
	return inference.GenerateStreamEvent{}, errStreamFailure
}
func (*failingStream) Close() error { return nil }

func TestConformanceGenerateStreamFailure(t *testing.T) {
	calls := &inferencetest.Counter{}
	model := "qwen-plus"
	driver, err := inference.BindGenerateStream(
		compileGenerate(model, catalog[model]),
		countingTransport(calls, func(_ context.Context, _ generateWire) (inference.ProviderStream[inference.GenerateStreamEvent], error) {
			return &failingStream{}, nil
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
		Model:          conformanceModel(model),
		Request:        conformanceTextRequest,
		Driver:         driver,
		TransportCalls: calls.Load,
		AssertError: func(t *testing.T, err error) {
			if !errors.Is(err, errStreamFailure) {
				t.Fatalf("stream error = %v", err)
			}
		},
	})
}
