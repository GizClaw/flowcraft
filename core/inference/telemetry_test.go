package inference

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/telemetry"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func installTestTracer(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	prev := otel.GetTracerProvider()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})
	return rec
}

func spanAttr(span sdktrace.ReadOnlySpan, key attribute.Key) (attribute.Value, bool) {
	for _, kv := range span.Attributes() {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func telemetryGenerateProvider(t *testing.T, respond func(GenerateRequest) (GenerateResponse, error)) *Assembly {
	t.Helper()
	return &Assembly{providers: map[string]ProviderDefinition{
		"fake": {
			ID: "fake",
			Models: []ModelImplementation{{
				Descriptor: ModelDescriptor{
					ID: ModelID{Provider: "fake", Name: "model-1"},
				},
				Openers: Openers{
					Generate: func(context.Context, ModelRef) (GenerateOperations, error) {
						return GenerateOperations{Unary: errGenerateDriver{respond: respond}}, nil
					},
				},
			}},
		},
	}}
}

type errGenerateDriver struct {
	respond func(GenerateRequest) (GenerateResponse, error)
}

func (errGenerateDriver) inferenceGenerateDriver() {}

func (d errGenerateDriver) Explain(
	context.Context, ModelRef, GenerateRequest,
) (Explanation, error) {
	return Explanation{}, nil
}

func (d errGenerateDriver) Execute(
	_ context.Context, _ ModelRef, req GenerateRequest,
) (GenerateResponse, error) {
	if d.respond != nil {
		return d.respond(req)
	}
	return GenerateResponse{}, nil
}

func TestAssemblyGenerateSuccessRecordsIDsOnActiveSpan(t *testing.T) {
	rec := installTestTracer(t)
	assembly := telemetryGenerateProvider(t, func(GenerateRequest) (GenerateResponse, error) {
		return GenerateResponse{
			Metadata: Metadata{RequestID: "req-1", ResponseID: "resp-1"},
			Usage:    testUsage(),
		}, nil
	})

	ctx, outer := telemetry.Tracer().Start(context.Background(), "outer")
	if _, err := assembly.Generate(ctx,
		ModelRef{ID: ModelID{Provider: "fake", Name: "model-1"}},
		GenerateRequest{}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	outer.End()

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1 (outer only, no nested inference span)", len(spans))
	}
	if got, ok := spanAttr(spans[0], telemetry.AttrLLMRequestID); !ok || got.AsString() != "req-1" {
		t.Fatalf("llm.request.id = %q/%v, want req-1", got.AsString(), ok)
	}
	if got, ok := spanAttr(spans[0], telemetry.AttrLLMResponseID); !ok || got.AsString() != "resp-1" {
		t.Fatalf("llm.response.id = %q/%v, want resp-1", got.AsString(), ok)
	}
	assertUsageAttrs(t, spans[0])
}

func TestAssemblyGenerateFailureRecordsRequestIDOnActiveSpan(t *testing.T) {
	rec := installTestTracer(t)
	assembly := telemetryGenerateProvider(t, func(GenerateRequest) (GenerateResponse, error) {
		return GenerateResponse{}, errdefs.WithRequestID(
			errdefs.Validation(errors.New("boom")), "req-err-1")
	})

	ctx, outer := telemetry.Tracer().Start(context.Background(), "outer")
	if _, err := assembly.Generate(ctx,
		ModelRef{ID: ModelID{Provider: "fake", Name: "model-1"}},
		GenerateRequest{}); !errdefs.IsValidation(err) {
		t.Fatalf("Generate error = %v, want validation", err)
	}
	outer.End()

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	if got, ok := spanAttr(spans[0], telemetry.AttrLLMRequestID); !ok || got.AsString() != "req-err-1" {
		t.Fatalf("llm.request.id = %q/%v, want req-err-1", got.AsString(), ok)
	}
	if _, ok := spanAttr(spans[0], telemetry.AttrLLMResponseID); ok {
		t.Fatal("llm.response.id should be absent on failure")
	}
}

func TestAssemblyGenerateStandaloneCreatesInferenceSpan(t *testing.T) {
	rec := installTestTracer(t)
	assembly := telemetryGenerateProvider(t, func(GenerateRequest) (GenerateResponse, error) {
		return GenerateResponse{
			Metadata: Metadata{RequestID: "req-1", ResponseID: "resp-1"},
			Usage:    testUsage(),
		}, nil
	})

	if _, err := assembly.Generate(context.Background(),
		ModelRef{ID: ModelID{Provider: "fake", Name: "model-1"}},
		GenerateRequest{}); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	if spans[0].Name() != "inference.generate" {
		t.Fatalf("span name = %q, want inference.generate", spans[0].Name())
	}
	if got, ok := spanAttr(spans[0], telemetry.AttrLLMRequestID); !ok || got.AsString() != "req-1" {
		t.Fatalf("llm.request.id = %q/%v, want req-1", got.AsString(), ok)
	}
	if got, ok := spanAttr(spans[0], telemetry.AttrLLMResponseID); !ok || got.AsString() != "resp-1" {
		t.Fatalf("llm.response.id = %q/%v, want resp-1", got.AsString(), ok)
	}
	if got, ok := spanAttr(spans[0], telemetry.AttrLLMProvider); !ok || got.AsString() != "fake" {
		t.Fatalf("llm.provider = %q/%v, want fake", got.AsString(), ok)
	}
	if got, ok := spanAttr(spans[0], telemetry.AttrLLMModel); !ok || got.AsString() != "model-1" {
		t.Fatalf("llm.model = %q/%v, want model-1", got.AsString(), ok)
	}
	assertUsageAttrs(t, spans[0])
}

func testUsage() Usage {
	cacheRead := int64(8)
	cacheWrite := int64(4)
	return Usage{
		InputTokens:  10,
		OutputTokens: 3,
		TotalTokens:  13,
		Model:        ModelRef{ID: ModelID{Provider: "fake", Name: "model-1"}},
		LatencyMs:    42,
		Input: InputTokenUsage{
			CacheReadTokens:  &cacheRead,
			CacheWriteTokens: &cacheWrite,
		},
		Billing: &BillingUsage{Cost: &Money{Currency: "usd", Units: 150, Scale: 4}},
	}
}

func assertUsageAttrs(t *testing.T, span sdktrace.ReadOnlySpan) {
	t.Helper()
	want := map[attribute.Key]int64{
		telemetry.AttrLLMInputTokens:       10,
		telemetry.AttrLLMOutputTokens:      3,
		telemetry.AttrLLMTotalTokens:       13,
		telemetry.AttrLLMCachedInputTokens: 8,
		telemetry.AttrLLMLatencyMs:         42,
		telemetry.AttrLLMCostMicros:        15000,
	}
	for key, wantValue := range want {
		got, ok := spanAttr(span, key)
		if !ok || got.AsInt64() != wantValue {
			t.Errorf("attr %s = %v/%v, want %d", key, got, ok, wantValue)
		}
	}
}

type telemetryFakeStream struct {
	events []GenerateStreamEvent
	next   int
	result GenerateResponse
}

func (s *telemetryFakeStream) Next(context.Context) (GenerateStreamEvent, error) {
	if s.next < len(s.events) {
		event := s.events[s.next]
		s.next++
		return event, nil
	}
	return GenerateStreamEvent{}, io.EOF
}

func (s *telemetryFakeStream) Result() (GenerateResponse, error) {
	return s.result, nil
}

func (*telemetryFakeStream) Close() error { return nil }

func TestTelemetryGenerateStreamRecordsIDsAtResult(t *testing.T) {
	rec := installTestTracer(t)
	_, call := startInferenceCall(
		context.Background(), OperationGenerate,
		ModelRef{ID: ModelID{Provider: "fake", Name: "model-1"}},
	)
	stream := &telemetryGenerateStream{
		inner: &telemetryFakeStream{
			events: []GenerateStreamEvent{{PartIndex: 0, Delta: TextPartDelta{Text: "hi"}}},
			result: GenerateResponse{
				Metadata: Metadata{RequestID: "req-1", ResponseID: "resp-1"},
				Usage:    testUsage(),
			},
		},
		tel: call,
	}

	for {
		_, err := stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
	if _, err := stream.Result(); err != nil {
		t.Fatalf("Result: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	if got, ok := spanAttr(spans[0], telemetry.AttrLLMRequestID); !ok || got.AsString() != "req-1" {
		t.Fatalf("llm.request.id = %q/%v, want req-1", got.AsString(), ok)
	}
	if got, ok := spanAttr(spans[0], telemetry.AttrLLMResponseID); !ok || got.AsString() != "resp-1" {
		t.Fatalf("llm.response.id = %q/%v, want resp-1", got.AsString(), ok)
	}
	assertUsageAttrs(t, spans[0])
}

func TestAssemblyGenerateEmitsUsageMetrics(t *testing.T) {
	prev := otel.GetMeterProvider()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(mp)
	t.Cleanup(func() {
		_ = mp.Shutdown(context.Background())
		otel.SetMeterProvider(prev)
	})

	assembly := telemetryGenerateProvider(t, func(GenerateRequest) (GenerateResponse, error) {
		return GenerateResponse{Usage: testUsage()}, nil
	})
	ref := ModelRef{ID: ModelID{Provider: "fake", Name: "model-1"}}
	if _, err := assembly.Generate(context.Background(), ref, GenerateRequest{}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	failing := telemetryGenerateProvider(t, func(GenerateRequest) (GenerateResponse, error) {
		return GenerateResponse{}, errdefs.WithRequestID(
			errdefs.Validation(errors.New("boom")), "req-err-1")
	})
	if _, err := failing.Generate(context.Background(), ref, GenerateRequest{}); !errdefs.IsValidation(err) {
		t.Fatalf("Generate error = %v, want validation", err)
	}

	var resources metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &resources); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	wantTokens := map[string]int64{
		"tokens.input":        10,
		"tokens.output":       3,
		"tokens.input.cached": 8,
	}
	for name, want := range wantTokens {
		if got := metricSum(resources, name, nil); got != want {
			t.Errorf("metric %s = %d, want %d", name, got, want)
		}
	}
	if got := metricSum(resources, "executions.total",
		func(set attribute.Set) bool {
			value, ok := set.Value(attribute.Key("status"))
			return ok && value.AsString() == "success"
		},
	); got != 1 {
		t.Errorf("executions.total success = %d, want 1", got)
	}
	if got := metricSum(resources, "executions.total",
		func(set attribute.Set) bool {
			value, ok := set.Value(attribute.Key("status"))
			return ok && value.AsString() == "error"
		},
	); got != 1 {
		t.Errorf("executions.total error = %d, want 1", got)
	}
	if got := metricSum(resources, "errors.total", nil); got != 1 {
		t.Errorf("errors.total = %d, want 1", got)
	}
}

func metricSum(
	resources metricdata.ResourceMetrics,
	name string,
	filter func(attribute.Set) bool,
) int64 {
	var total int64
	for _, scope := range resources.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, point := range sum.DataPoints {
				if filter == nil || filter(point.Attributes) {
					total += point.Value
				}
			}
		}
	}
	return total
}
