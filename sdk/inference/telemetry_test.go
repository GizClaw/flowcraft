package inference

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	"github.com/GizClaw/flowcraft/sdk/message"
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
				Message: message.Message{
					Role:    message.RoleAssistant,
					Content: message.Content{Parts: []message.Part{message.TextPart{Text: "ok"}}},
				},
				FinishReason: FinishCompleted,
				Usage: Usage{
					InputTokens:  10,
					OutputTokens: 5,
					TotalTokens:  15,
					Input:        InputTokenUsage{CacheReadTokens: &cached},
					LatencyMs:    42,
					Billing: &BillingUsage{Cost: &Money{
						Currency: "USD",
						Units:    12345,
						Scale:    6,
					}},
				},
				Metadata: Metadata{
					RequestID:  "req-1",
					ResponseID: "resp-1",
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
		telemetry.AttrLLMLatencyMs:         int64(42),
		telemetry.AttrLLMCostMicros:        int64(12345),
		telemetry.AttrLLMRequestID:         "req-1",
		telemetry.AttrLLMResponseID:        "resp-1",
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
	runtime := newTelemetryRuntime(t,
		errdefs.WithRequestID(errors.New("connection reset"), "req-fail"),
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
	if got := attrs[telemetry.AttrLLMRequestID].AsString(); got != "req-fail" {
		t.Fatalf("request id attr = %q, want req-fail", got)
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
			{
				FinishReason: FinishCompleted,
				RequestID:    "req-stream",
				ResponseID:   "resp-stream",
			},
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
	if got := attrs[telemetry.AttrLLMRequestID].AsString(); got != "req-stream" {
		t.Fatalf("request id attr = %q, want req-stream", got)
	}
	if got := attrs[telemetry.AttrLLMResponseID].AsString(); got != "resp-stream" {
		t.Fatalf("response id attr = %q, want resp-stream", got)
	}
}

func TestRuntimeStampsUsageEnvelope(t *testing.T) {
	installSpanRecorder(t)
	runtime := newTelemetryRuntime(t, nil,
		func(context.Context, string) (GenerateResponse, error) {
			return GenerateResponse{
				Message: message.Message{
					Role:    message.RoleAssistant,
					Content: message.Content{Parts: []message.Part{message.TextPart{Text: "ok"}}},
				},
				FinishReason: FinishCompleted,
				Usage:        Usage{InputTokens: 2, OutputTokens: 2, TotalTokens: 4},
			}, nil
		},
		[]GenerateStreamEvent{
			{Usage: &Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}},
			{PartIndex: 0, Delta: TextPartDelta{Text: "ok"}},
			{
				FinishReason: FinishCompleted,
				RequestID:    "req-stream",
				ResponseID:   "resp-stream",
			},
		},
	)
	resp, err := runtime.Generate(context.Background(), telemetryModelRef(), validGenerateTextRequest())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Usage.Model != telemetryModelRef() {
		t.Fatalf("stamped model = %+v, want %+v", resp.Usage.Model, telemetryModelRef())
	}
	if resp.Usage.LatencyMs < 0 {
		t.Fatalf("stamped latency = %d, must be non-negative", resp.Usage.LatencyMs)
	}

	stream, err := runtime.GenerateStream(context.Background(), telemetryModelRef(), validGenerateTextRequest())
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	event, err := stream.Next(context.Background())
	if err != nil {
		t.Fatalf("stream.Next: %v", err)
	}
	if event.Usage == nil || event.Usage.Model != telemetryModelRef() {
		t.Fatalf("stream usage snapshot not stamped: %+v", event.Usage)
	}
	for {
		if _, nextErr := stream.Next(context.Background()); errors.Is(nextErr, io.EOF) {
			break
		} else if nextErr != nil {
			t.Fatalf("stream.Next: %v", nextErr)
		}
	}
	result, err := stream.Result()
	if err != nil {
		t.Fatalf("stream.Result: %v", err)
	}
	if result.Usage.Model != telemetryModelRef() {
		t.Fatalf("result usage not stamped: %+v", result.Usage.Model)
	}
	if result.Metadata.RequestID != "req-stream" ||
		result.Metadata.ResponseID != "resp-stream" {
		t.Fatalf("result ids = %+v", result.Metadata)
	}
	_ = stream.Close()
}

func TestRuntimeStampPreservesProviderEnvelope(t *testing.T) {
	installSpanRecorder(t)
	other := ModelRef{ID: ModelID{Provider: "fake", Name: "server-side"}}
	runtime := newTelemetryRuntime(t, nil,
		func(context.Context, string) (GenerateResponse, error) {
			return GenerateResponse{
				Message: message.Message{
					Role:    message.RoleAssistant,
					Content: message.Content{Parts: []message.Part{message.TextPart{Text: "ok"}}},
				},
				FinishReason: FinishCompleted,
				Usage: Usage{
					InputTokens: 2, OutputTokens: 2, TotalTokens: 4,
					// A provider that reports its own envelope knows
					// better than the Runtime's wall clock.
					Model:     other,
					LatencyMs: 1234,
				},
			}, nil
		},
		nil,
	)
	resp, err := runtime.Generate(context.Background(), telemetryModelRef(), validGenerateTextRequest())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Usage.Model != other {
		t.Fatalf("provider model overwritten: %+v, want %+v", resp.Usage.Model, other)
	}
	if resp.Usage.LatencyMs != 1234 {
		t.Fatalf("provider latency overwritten: %d, want 1234", resp.Usage.LatencyMs)
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

func TestBillingCostMicros(t *testing.T) {
	cost := func(units int64, scale uint8) *BillingUsage {
		return &BillingUsage{Cost: &Money{Currency: "USD", Units: units, Scale: scale}}
	}

	tests := []struct {
		name    string
		billing *BillingUsage
		want    int64
		ok      bool
	}{
		{name: "nil", billing: nil, ok: false},
		{name: "no cost", billing: &BillingUsage{}, ok: false},
		{name: "zero cost", billing: cost(0, 6), ok: false},
		{name: "micros scale", billing: cost(12345, 6), want: 12345, ok: true},
		{name: "cents to micros", billing: cost(123, 2), want: 1230000, ok: true},
		{name: "dollars to micros", billing: cost(12, 0), want: 12000000, ok: true},
		{name: "xai tenths of micro-USD", billing: cost(123456, 10), want: 12, ok: true},
		{name: "sub-micro floors to zero", billing: cost(5, 7), ok: false},
		{name: "invalid scale", billing: cost(1, 19), ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := billingCostMicros(tt.billing)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if ok && got != tt.want {
				t.Fatalf("micros = %d, want %d", got, tt.want)
			}
		})
	}
}
