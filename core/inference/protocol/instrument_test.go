package protocol_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/telemetry/telemetrytest"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// bindGenerateDriver builds a unary driver whose decoder produces the response
// (and, for failure cases, the classified error).
func bindGenerateDriver(t *testing.T, decode func() (inference.GenerateResponse, error)) protocol.GenerateDriver {
	t.Helper()
	driver, err := protocol.BindGenerate(
		"fake",
		func(_ context.Context, r inference.GenerateRequest, _ protocol.Shape) (protocol.Compiled[string], error) {
			return protocol.Compiled[string]{Wire: "wire", Fields: r.ActiveFields()}, nil
		},
		func(context.Context, inference.GenerateRequest, string) (string, error) {
			return "raw", nil
		},
		func(context.Context, inference.GenerateRequest, string) (inference.GenerateResponse, error) {
			return decode()
		},
	)
	if err != nil {
		t.Fatalf("bind generate: %v", err)
	}
	return driver
}

func int64Ptr(value int64) *int64 { return &value }

// TestUnaryCallEmitsOneSpan pins the contract of the funnel: one span per
// provider round trip, named llm.generate, carrying the GenAI conventions for the
// facts they define and llm.* for the facts they do not.
func TestUnaryCallEmitsOneSpan(t *testing.T) {
	recorder := telemetrytest.New(t)

	driver := bindGenerateDriver(t, func() (inference.GenerateResponse, error) {
		response := unaryResponse("ok")
		response.Usage = usage.Usage{
			InputTokens:  10,
			OutputTokens: 4,
			Input:        usage.InputTokenUsage{CacheReadTokens: int64Ptr(6)},
			Billing: &usage.Billing{
				Cost: &usage.Money{Currency: "USD", Units: 25, Scale: 6},
			},
		}
		response.Metadata = inference.Metadata{
			Model:      model.ModelID{Provider: "fake", Name: "m1-resolved"},
			RequestID:  "req-1",
			ResponseID: "resp-1",
		}
		return response, nil
	})
	if _, err := driver.Execute(context.Background(), request(t)); err != nil {
		t.Fatalf("execute: %v", err)
	}

	span := recorder.RequireNamed(t, "llm.generate")
	telemetrytest.RequireAttrString(t, span, telemetry.AttrGenAIOperationName, telemetry.GenAIOperationChat)
	telemetrytest.RequireAttrString(t, span, telemetry.AttrGenAIProviderName, "fake")
	telemetrytest.RequireAttrString(t, span, telemetry.AttrGenAIRequestModel, "m1")
	telemetrytest.RequireAttrString(t, span, telemetry.AttrInferenceShape, "unary")
	telemetrytest.RequireAttrString(t, span, telemetry.AttrGenAIResponseModel, "m1-resolved")
	telemetrytest.RequireAttrString(t, span, telemetry.AttrGenAIResponseID, "resp-1")
	telemetrytest.RequireAttrString(t, span, telemetry.AttrLLMRequestID, "req-1")
	telemetrytest.RequireAttr(t, span, telemetry.AttrGenAIUsageInputTokens, attribute.Int64Value(10))
	telemetrytest.RequireAttr(t, span, telemetry.AttrGenAIUsageOutputTokens, attribute.Int64Value(4))
	telemetrytest.RequireAttr(t, span, telemetry.AttrGenAIUsageCacheReadInputTokens, attribute.Int64Value(6))
	telemetrytest.RequireAttr(t, span, telemetry.AttrLLMCostMicros, attribute.Int64Value(25))

	reasons := telemetrytest.Attrs(span)[telemetry.AttrGenAIResponseFinishReasons].AsStringSlice()
	if len(reasons) != 1 || reasons[0] != string(inference.FinishStop) {
		t.Fatalf("finish reasons = %v, want [stop]", reasons)
	}
	if span.Status().Code != codes.Ok {
		t.Fatalf("status = %v, want Ok", span.Status().Code)
	}
}

// TestUnaryCallRecordsErrorKind pins that a failure is classified on the span and
// that the payload-bearing message is not copied onto it.
func TestUnaryCallRecordsErrorKind(t *testing.T) {
	recorder := telemetrytest.New(t)

	driver := bindGenerateDriver(t, func() (inference.GenerateResponse, error) {
		return inference.GenerateResponse{}, inference.NewError(
			inference.ProviderFailure, "",
			errors.New("upstream said: secret prompt"),
		).WithDetail("transport.do")
	})
	if _, err := driver.Execute(context.Background(), request(t)); err == nil {
		t.Fatal("expected an error")
	}

	span := recorder.RequireNamed(t, "llm.generate")
	telemetrytest.RequireAttrString(t, span, telemetry.AttrErrorKind, string(inference.ProviderFailure))
	telemetrytest.RequireAttrString(t, span, telemetry.AttrInferenceErrorDetail, "transport.do")
	if span.Status().Code != codes.Error {
		t.Fatalf("status = %v, want Error", span.Status().Code)
	}
	if description := span.Status().Description; description != string(inference.ProviderFailure) {
		t.Fatalf("status description = %q, want only the kind", description)
	}
	if events := span.Events(); len(events) != 0 {
		t.Fatalf("error path recorded %d event(s); the cause must not be copied to the span", len(events))
	}
}

// TestStreamSpanWaitsForResult pins that the stream's span ends at the terminal
// event, so the assembled finish reason is on it, and that the first text delta
// is timed exactly once.
func TestStreamSpanWaitsForResult(t *testing.T) {
	recorder := telemetrytest.New(t)
	driver, _ := streamDriver(t, "he", "llo", "end")

	stream, err := driver.Open(context.Background(), request(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for {
		if _, err := stream.Next(context.Background()); err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("next: %v", err)
			}
			break
		}
	}
	response, err := stream.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if response.FinishReason != inference.FinishStop {
		t.Fatalf("finish reason = %q", response.FinishReason)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	recorder.RequireCount(t, "llm.generate", 1)
	span := recorder.RequireNamed(t, "llm.generate")
	telemetrytest.RequireAttrString(t, span, telemetry.AttrInferenceShape, "stream")
	reasons := telemetrytest.Attrs(span)[telemetry.AttrGenAIResponseFinishReasons].AsStringSlice()
	if len(reasons) != 1 || reasons[0] != string(inference.FinishStop) {
		t.Fatalf("finish reasons = %v, want [stop]", reasons)
	}
	telemetrytest.RequireEventCount(t, span, "llm.first_token", 1)
	if span.Status().Code != codes.Ok {
		t.Fatalf("status = %v, want Ok", span.Status().Code)
	}
}

// TestStreamClosedEarlyIsTruncated pins the classification of an abandoned
// stream: the caller ended it, so it is a truncation, not a provider failure.
func TestStreamClosedEarlyIsTruncated(t *testing.T) {
	recorder := telemetrytest.New(t)
	driver, _ := streamDriver(t, "he", "llo", "end")

	stream, err := driver.Open(context.Background(), request(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := stream.Next(context.Background()); err != nil {
		t.Fatalf("next: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	recorder.RequireCount(t, "llm.generate", 1)
	span := recorder.RequireNamed(t, "llm.generate")
	telemetrytest.RequireAttrString(t, span, telemetry.AttrErrorKind, string(inference.ProviderTruncated))
	telemetrytest.RequireAttrString(t, span, telemetry.AttrInferenceErrorDetail, "stream.closed.early")
	if span.Status().Code != codes.Error {
		t.Fatalf("status = %v, want Error", span.Status().Code)
	}
}

// TestStreamDecodeFailureClosesSpanOnce pins that a mid-stream failure and the
// Close that follows it produce one span, not two.
func TestStreamDecodeFailureClosesSpanOnce(t *testing.T) {
	recorder := telemetrytest.New(t)

	driver, err := protocol.BindGenerateStream(
		"fake",
		func(_ context.Context, r inference.GenerateRequest, _ protocol.Shape) (protocol.Compiled[string], error) {
			return protocol.Compiled[string]{Wire: "wire", Fields: r.ActiveFields()}, nil
		},
		func(context.Context, inference.GenerateRequest, string) (protocol.RawStream[string], error) {
			return &fakeStream{chunks: []string{"he", "boom"}}, nil
		},
		func(inference.GenerateRequest) protocol.StreamDecoder[string] {
			return func(_ context.Context, chunk string) ([]inference.StreamEvent, error) {
				if chunk == "boom" {
					return nil, errors.New("undecodable chunk")
				}
				return []inference.StreamEvent{{Kind: inference.StreamText, Text: chunk}}, nil
			}
		},
	)
	if err != nil {
		t.Fatalf("bind stream: %v", err)
	}

	stream, err := driver.Open(context.Background(), request(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for {
		if _, nextErr := stream.Next(context.Background()); nextErr != nil {
			break
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	recorder.RequireCount(t, "llm.generate", 1)
	span := recorder.RequireNamed(t, "llm.generate")
	telemetrytest.RequireAttrString(t, span, telemetry.AttrErrorKind, string(inference.InvalidProviderResponse))
}

// TestTokenAndDurationMetrics pins the metric side: the counters are keyed by the
// low-cardinality identity, and only reported token counts move.
func TestTokenAndDurationMetrics(t *testing.T) {
	reader := installMeter(t)

	driver := bindGenerateDriver(t, func() (inference.GenerateResponse, error) {
		response := unaryResponse("ok")
		response.Usage = usage.Usage{
			InputTokens:  10,
			OutputTokens: 4,
			Input:        usage.InputTokenUsage{CacheReadTokens: int64Ptr(6)},
		}
		return response, nil
	})
	if _, err := driver.Execute(context.Background(), request(t)); err != nil {
		t.Fatalf("execute: %v", err)
	}

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("collect: %v", err)
	}

	if got := counterSum(t, collected, "executions.total"); got != 1 {
		t.Errorf("executions.total = %d, want 1", got)
	}
	if got := counterSum(t, collected, "tokens.input"); got != 10 {
		t.Errorf("tokens.input = %d, want 10", got)
	}
	if got := counterSum(t, collected, "tokens.output"); got != 4 {
		t.Errorf("tokens.output = %d, want 4", got)
	}
	if got := counterSum(t, collected, "tokens.input.cached"); got != 6 {
		t.Errorf("tokens.input.cached = %d, want 6", got)
	}
	if got := histogramCount(t, collected, "duration.seconds"); got != 1 {
		t.Errorf("duration.seconds count = %d, want 1", got)
	}
}

// installMeter points the global meter provider at a manual reader. Instruments
// are resolved per call, so a provider installed before the call is the one that
// receives the measurement.
func installMeter(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	previous := otel.GetMeterProvider()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetMeterProvider(previous)
	})
	return reader
}

func counterSum(t *testing.T, collected metricdata.ResourceMetrics, name string) int64 {
	t.Helper()
	for _, scope := range collected.ScopeMetrics {
		for _, record := range scope.Metrics {
			if record.Name != name {
				continue
			}
			sum, ok := record.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is not an int64 sum", name)
			}
			var total int64
			for _, point := range sum.DataPoints {
				total += point.Value
			}
			return total
		}
	}
	t.Fatalf("metric %q not recorded", name)
	return 0
}

func histogramCount(t *testing.T, collected metricdata.ResourceMetrics, name string) uint64 {
	t.Helper()
	for _, scope := range collected.ScopeMetrics {
		for _, record := range scope.Metrics {
			if record.Name != name {
				continue
			}
			histogram, ok := record.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("%s is not a float64 histogram", name)
			}
			var total uint64
			for _, point := range histogram.DataPoints {
				total += point.Count
			}
			return total
		}
	}
	t.Fatalf("metric %q not recorded", name)
	return 0
}
