package inference

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/telemetry"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func installSpanRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		_ = provider.Shutdown(context.Background())
	})
	return recorder
}

func telemetrySpanAttrs(t *testing.T, span sdktrace.ReadOnlySpan) map[string]attribute.Value {
	t.Helper()
	attrs := make(map[string]attribute.Value, len(span.Attributes()))
	for _, attr := range span.Attributes() {
		attrs[string(attr.Key)] = attr.Value
	}
	return attrs
}

func newTelemetryRuntime(
	t *testing.T,
	transportErr error,
	unary func(context.Context, string) (GenerateResponse, error),
	streamEvents []GenerateStreamEvent,
) *Runtime {
	t.Helper()
	operations, err := BindGenerateOperations(
		nativeGenerateCompile("wire"),
		func(context.Context, string) (string, error) {
			if transportErr != nil {
				return "", transportErr
			}
			return "response", nil
		},
		unary,
		func(context.Context, string) (ProviderStream[GenerateStreamEvent], error) {
			return &generateEventStream{events: streamEvents}, nil
		},
		func(_ context.Context, event GenerateStreamEvent) (GenerateStreamEvent, error) {
			return event, nil
		},
	)
	if err != nil {
		t.Fatalf("BindGenerateOperations: %v", err)
	}
	runtime, err := NewRuntime([]ProviderDefinition{{
		ID: "fake",
		Models: []ModelImplementation{{
			Descriptor: ModelDescriptor{ID: ModelID{Provider: "fake", Name: "omni"}},
			Openers: Openers{
				Generate: func(context.Context, ModelRef) (GenerateOperations, error) {
					return operations, nil
				},
			},
		}},
	}})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	return runtime
}

func telemetryModelRef() ModelRef {
	return ModelRef{ID: ModelID{Provider: "fake", Name: "omni"}}
}

func TestRuntimeTelemetryGenerateSuccess(t *testing.T) {
	recorder := installSpanRecorder(t)
	cached := int64(4)
	runtime := newTelemetryRuntime(t, nil,
		func(context.Context, string) (GenerateResponse, error) {
			return GenerateResponse{
				Message: Message{
					Role:    RoleAssistant,
					Content: Content{Parts: []Part{TextPart{Text: "ok"}}},
				},
				FinishReason: FinishCompleted,
				Usage: Usage{
					InputTokens:  10,
					OutputTokens: 5,
					TotalTokens:  15,
					Input:        InputTokenUsage{CacheReadTokens: &cached},
				},
			}, nil
		},
		nil,
	)
	if _, err := runtime.Generate(context.Background(), telemetryModelRef(), validGenerateTextRequest()); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Name() != "inference.generate" {
		t.Fatalf("span name = %q", span.Name())
	}
	if span.Status().Code != codes.Ok {
		t.Fatalf("span status = %v, want Ok", span.Status())
	}
	attrs := telemetrySpanAttrs(t, span)
	for key, want := range map[string]any{
		"inference.operation":              "generate",
		telemetry.AttrLLMProvider:          "fake",
		telemetry.AttrLLMModel:             "omni",
		telemetry.AttrLLMInputTokens:       int64(10),
		telemetry.AttrLLMOutputTokens:      int64(5),
		telemetry.AttrLLMTotalTokens:       int64(15),
		telemetry.AttrLLMCachedInputTokens: int64(4),
	} {
		attr, ok := attrs[key]
		if !ok {
			t.Fatalf("missing span attr %q", key)
		}
		var got any
		switch want.(type) {
		case string:
			got = attr.AsString()
		case int64:
			got = attr.AsInt64()
		}
		if got != want {
			t.Fatalf("attr %q = %v, want %v", key, got, want)
		}
	}
}

func TestRuntimeTelemetryGenerateFailure(t *testing.T) {
	recorder := installSpanRecorder(t)
	runtime := newTelemetryRuntime(t, errors.New("connection reset"),
		func(context.Context, string) (GenerateResponse, error) {
			return GenerateResponse{}, errors.New("decode unused")
		},
		nil,
	)
	_, err := runtime.Generate(context.Background(), telemetryModelRef(), validGenerateTextRequest())
	if err == nil {
		t.Fatal("Generate should fail")
	}
	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Status().Code != codes.Error {
		t.Fatalf("span status = %v, want Error", span.Status())
	}
	attrs := telemetrySpanAttrs(t, span)
	if got := attrs["inference.error_kind"].AsString(); got != string(ProviderFailure) {
		t.Fatalf("error_kind = %q, want %q", got, ProviderFailure)
	}
}

func TestRuntimeTelemetryStreamClosesSpanOnEOF(t *testing.T) {
	recorder := installSpanRecorder(t)
	var decodeCalls atomic.Int32
	usage := &Usage{InputTokens: 7, OutputTokens: 3, TotalTokens: 10}
	runtime := newTelemetryRuntime(t, nil,
		func(context.Context, string) (GenerateResponse, error) {
			return GenerateResponse{}, errors.New("unary unused")
		},
		[]GenerateStreamEvent{
			{PartIndex: 0, Delta: TextPartDelta{Text: "ok"}},
			{Usage: usage},
			{FinishReason: FinishCompleted},
		},
	)
	stream, err := runtime.GenerateStream(context.Background(), telemetryModelRef(), validGenerateTextRequest())
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	// The span must stay open while the stream is live.
	if got := len(recorder.Ended()); got != 0 {
		t.Fatalf("spans ended before stream EOF = %d", got)
	}
	for {
		event, nextErr := stream.Next(context.Background())
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatalf("stream.Next: %v", nextErr)
		}
		_ = event
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("stream.Close: %v", err)
	}
	if decodeCalls.Load() != 0 {
		t.Fatalf("decode calls = %d", decodeCalls.Load())
	}
	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Name() != "inference.generate.stream" {
		t.Fatalf("span name = %q", span.Name())
	}
	if span.Status().Code != codes.Ok {
		t.Fatalf("span status = %v, want Ok", span.Status())
	}
	attrs := telemetrySpanAttrs(t, span)
	if got := attrs[telemetry.AttrLLMTotalTokens].AsInt64(); got != 10 {
		t.Fatalf("total tokens attr = %d, want 10", got)
	}
}

func TestRuntimeTelemetryExplainStaysSilent(t *testing.T) {
	recorder := installSpanRecorder(t)
	runtime := newTelemetryRuntime(t, nil,
		func(context.Context, string) (GenerateResponse, error) {
			return GenerateResponse{}, errors.New("unary unused")
		},
		nil,
	)
	if _, err := runtime.ExplainGenerate(context.Background(), telemetryModelRef(), validGenerateTextRequest()); err != nil {
		t.Fatalf("ExplainGenerate: %v", err)
	}
	if got := len(recorder.Ended()); got != 0 {
		t.Fatalf("ExplainGenerate produced %d spans, want 0", got)
	}
}
