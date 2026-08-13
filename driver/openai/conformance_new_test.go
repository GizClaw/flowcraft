package openai

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/message"
)

func TestConformanceGenerateConcurrent(t *testing.T) {
	calls := &inferencetest.Counter{}
	unary, stream := conformanceGenerateDriversOpenAI(t, calls)
	inferencetest.RunGenerateConcurrent(t, inferencetest.GenerateConcurrentSuite{
		Model:      openaiModel("gpt-5.6-sol"),
		Request:    func() inference.GenerateRequest { return simpleTextRequest("hi") },
		Unary:      unary,
		Stream:     stream,
		Goroutines: 2,
	})
}

func conformanceGenerateDriversOpenAI(
	t *testing.T,
	calls *inferencetest.Counter,
) (inference.GenerateDriver, inference.GenerateStreamDriver) {
	t.Helper()
	model := "gpt-5.6-sol"
	operations, err := inference.BindGenerateOperations(
		compileGenerate(model, catalog[model]),
		countingTransport(calls, func(_ context.Context, wire generateWire) (generateWire, error) { return wire, nil }),
		inference.Decoder[generateWire, inference.GenerateResponse](
			func(_ context.Context, _ generateWire) (inference.GenerateResponse, error) {
				return inference.GenerateResponse{
					Message: message.Message{
						Role:    message.RoleAssistant,
						Content: message.Content{Parts: []message.Part{message.TextPart{Text: "ok"}}},
					},
					FinishReason: inference.FinishCompleted,
				}, nil
			},
		),
		countingTransport(calls, func(_ context.Context, _ generateWire) (inference.ProviderStream[inference.GenerateStreamEvent], error) {
			return &okOpenAIStream{events: []inference.GenerateStreamEvent{
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

type okOpenAIStream struct {
	events []inference.GenerateStreamEvent
	index  int
}

func (s *okOpenAIStream) Next(context.Context) (inference.GenerateStreamEvent, error) {
	if s.index >= len(s.events) {
		return inference.GenerateStreamEvent{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}
func (*okOpenAIStream) Close() error { return nil }

func TestConformanceEmbedUnary(t *testing.T) {
	calls := &inferencetest.Counter{}
	model := "text-embedding-3-large"
	driver, err := inference.BindEmbed(
		compileEmbed(model, catalog[model]),
		countingTransport(calls, func(_ context.Context, wire embedWire) (embedWire, error) { return wire, nil }),
		inference.Decoder[embedWire, inference.EmbedResponse](
			func(_ context.Context, _ embedWire) (inference.EmbedResponse, error) {
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
		Model: openaiModel(model),
		Request: func() inference.EmbedRequest {
			return inference.EmbedRequest{Items: []inference.EmbedItem{{
				Content: message.Content{Parts: []message.Part{message.TextPart{Text: "hi"}}},
			}}}
		},
		Driver:         driver,
		TransportCalls: calls.Load,
	})
}

var errOpenAIStreamFailure = errors.New("openai stream failure")

type failingOpenAIStream struct{}

func (*failingOpenAIStream) Next(context.Context) (inference.GenerateStreamEvent, error) {
	return inference.GenerateStreamEvent{}, errOpenAIStreamFailure
}
func (*failingOpenAIStream) Close() error { return nil }

func TestConformanceGenerateStreamFailure(t *testing.T) {
	calls := &inferencetest.Counter{}
	model := "gpt-5.6-sol"
	driver, err := inference.BindGenerateStream(
		compileGenerate(model, catalog[model]),
		countingTransport(calls, func(_ context.Context, _ generateWire) (inference.ProviderStream[inference.GenerateStreamEvent], error) {
			return &failingOpenAIStream{}, nil
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
		Model:          openaiModel(model),
		Request:        func() inference.GenerateRequest { return simpleTextRequest("hi") },
		Driver:         driver,
		TransportCalls: calls.Load,
		AssertError: func(t *testing.T, err error) {
			if !errors.Is(err, errOpenAIStreamFailure) {
				t.Fatalf("stream error = %v", err)
			}
		},
	})
}
