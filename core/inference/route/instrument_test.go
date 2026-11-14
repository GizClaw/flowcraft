package route

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/telemetry/telemetrytest"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func okResponse(text string) inference.GenerateResponse {
	return inference.GenerateResponse{
		Message:      message.Message{Role: message.RoleAssistant, Content: message.NewTextContent(text)},
		FinishReason: inference.FinishStop,
	}
}

// TestRouteSpanRecordsFailover pins the question the attempt spans cannot answer:
// how many candidates were tried, whether we fell back, and which one answered.
func TestRouteSpanRecordsFailover(t *testing.T) {
	recorder := telemetrytest.New(t)
	reader := installRouteMeter(t)

	first := &fakeGenerator{reply: func(int) (inference.GenerateResponse, error) {
		return inference.GenerateResponse{}, inference.NewError(
			inference.UnknownModel, inference.FieldModel, errors.New("not declared"))
	}}
	second := &fakeGenerator{reply: func(int) (inference.GenerateResponse, error) {
		return okResponse("ok"), nil
	}}
	router, err := New([]Target{generateTarget("a", first), generateTarget("b", second)}, Policy{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	response, err := router.Generate(context.Background(), generationRequest())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if response.Message.Content.Text() != "ok" {
		t.Fatalf("response came from the wrong target: %q", response.Message.Content.Text())
	}

	span := recorder.RequireNamed(t, "llm.route")
	telemetrytest.RequireAttrString(t, span, telemetry.AttrRouteSelected, "a")
	telemetrytest.RequireAttrString(t, span, telemetry.AttrRouteExecuted, "b")
	telemetrytest.RequireAttr(t, span, telemetry.AttrRouteAttempts, attribute.IntValue(2))
	telemetrytest.RequireAttr(t, span, telemetry.AttrRouteFallbacks, attribute.IntValue(1))
	telemetrytest.RequireAttrString(t, span, telemetry.AttrInferenceOperation, operationGenerate)
	telemetrytest.RequireEventCount(t, span, "route.attempt", 2)
	if span.Status().Code != codes.Ok {
		t.Fatalf("status = %v, want Ok", span.Status().Code)
	}

	// A failover must not look like two model calls: the GenAI vocabulary stays
	// on the attempt span.
	for key := range telemetrytest.Attrs(span) {
		if strings.HasPrefix(key, "gen_ai.") {
			t.Errorf("route span carries %q; gen_ai.* belongs to the attempt span", key)
		}
	}

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if got := routeCounterSum(t, collected, "fallbacks.total"); got != 1 {
		t.Errorf("fallbacks.total = %d, want 1", got)
	}
	if got := routeCounterSum(t, collected, "executions.total"); got != 1 {
		t.Errorf("executions.total = %d, want 1", got)
	}
}

// TestRouteSpanRecordsRetry pins the same-target retry path: the attempt count
// grows but no fallback happened, because the dispatcher never left the target.
func TestRouteSpanRecordsRetry(t *testing.T) {
	recorder := telemetrytest.New(t)

	flaky := &fakeGenerator{reply: func(call int) (inference.GenerateResponse, error) {
		if call == 1 {
			return inference.GenerateResponse{}, inference.NewError(
				inference.ProviderFailure, "", errors.New("transient"))
		}
		return okResponse("ok"), nil
	}}
	router, err := New([]Target{generateTarget("a", flaky)}, Policy{
		Retry: RetryPolicy{AttemptsPerTarget: 2},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := router.Generate(context.Background(), generationRequest()); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	span := recorder.RequireNamed(t, "llm.route")
	telemetrytest.RequireAttr(t, span, telemetry.AttrRouteAttempts, attribute.IntValue(2))
	telemetrytest.RequireAttr(t, span, telemetry.AttrRouteRetries, attribute.IntValue(1))
	telemetrytest.RequireAttrString(t, span, telemetry.AttrRouteExecuted, "a")
	if _, ok := telemetrytest.Attrs(span)[telemetry.AttrRouteFallbacks]; ok {
		t.Fatal("a same-target retry must not be reported as a fallback")
	}
}

// TestRouteSpanRecordsExhaustion pins the all-targets-failed shape: the span
// carries the attempts and the classified reason.
func TestRouteSpanRecordsExhaustion(t *testing.T) {
	recorder := telemetrytest.New(t)

	failing := &fakeGenerator{reply: func(int) (inference.GenerateResponse, error) {
		return inference.GenerateResponse{}, inference.NewError(
			inference.ProviderFailure, "", errors.New("down"))
	}}
	router, err := New([]Target{generateTarget("a", failing)}, Policy{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := router.Generate(context.Background(), generationRequest()); err == nil {
		t.Fatal("expected an error")
	}

	span := recorder.RequireNamed(t, "llm.route")
	telemetrytest.RequireAttr(t, span, telemetry.AttrRouteAttempts, attribute.IntValue(1))
	telemetrytest.RequireAttrString(t, span, telemetry.AttrErrorKind, string(inference.ProviderFailure))
	if span.Status().Code != codes.Error {
		t.Fatalf("status = %v, want Error", span.Status().Code)
	}
}

// TestRouteSpanDoesNotRecordUsage pins the division of labour: the route span
// describes the decision, not the call, so it carries no token counts.
func TestRouteSpanDoesNotRecordUsage(t *testing.T) {
	recorder := telemetrytest.New(t)

	answering := &fakeGenerator{reply: func(int) (inference.GenerateResponse, error) {
		response := okResponse("ok")
		response.Usage = usage.Usage{InputTokens: 3, OutputTokens: 2}
		return response, nil
	}}
	router, err := New([]Target{generateTarget("a", answering)}, Policy{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := router.Generate(context.Background(), generationRequest()); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	attrs := telemetrytest.Attrs(recorder.RequireNamed(t, "llm.route"))
	for _, key := range []string{
		telemetry.AttrGenAIUsageInputTokens,
		telemetry.AttrGenAIUsageOutputTokens,
		telemetry.AttrLLMCostMicros,
	} {
		if _, ok := attrs[key]; ok {
			t.Errorf("route span carries %q; usage belongs to the attempt span", key)
		}
	}
}

// installRouteMeter points the global meter provider at a manual reader.
func installRouteMeter(t *testing.T) *sdkmetric.ManualReader {
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

func routeCounterSum(t *testing.T, collected metricdata.ResourceMetrics, name string) int64 {
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
