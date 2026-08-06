package route

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func installRouteSpanRecorder(t *testing.T) *tracetest.SpanRecorder {
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

func routeSpanAttr(span sdktrace.ReadOnlySpan, key string) (attribute.Value, bool) {
	for _, attr := range span.Attributes() {
		if string(attr.Key) == key {
			return attr.Value, true
		}
	}
	return attribute.Value{}, false
}

func TestRouteTelemetryRecordsFallbackJourney(t *testing.T) {
	recorder := installRouteSpanRecorder(t)
	first, second := generateModel("primary"), generateModel("backup")
	runtime := newGenerateRouteRuntime(t, map[string]generateRouteBehavior{
		// Compiler rejections are the fallback-eligible failures; a
		// transport failure never migrates by design.
		"primary": {reject: inference.UnsupportedFeature},
		"backup":  {},
	})
	router := newGenerateRouter(t, runtime, first, generateFallbackFunc(func(
		_ context.Context,
		_ inference.GenerateRequest,
		attempt Attempt,
	) (inference.ModelRef, bool, error) {
		if attempt.Target != first {
			t.Fatalf("fallback attempt target = %+v, want primary", attempt.Target)
		}
		return second, true, nil
	}))

	response, trace, err := router.Generate(context.Background(), generateRequest("hi"))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if trace.Executed != second || response.Metadata.Model != second.ID {
		t.Fatalf("executed = %+v, want backup", trace.Executed)
	}

	// One route span, plus one child span for the backup attempt's
	// Runtime.Generate call. The failed primary attempt was rejected at
	// preflight (Explain), which is not instrumented.
	spans := recorder.Ended()
	var routeSpan sdktrace.ReadOnlySpan
	runtimeSpans := 0
	for _, span := range spans {
		if span.Name() == "inference.route.generate" {
			routeSpan = span
			continue
		}
		if span.Name() == "inference.generate" {
			runtimeSpans++
		}
	}
	if routeSpan == nil {
		t.Fatal("route span not found")
	}
	if runtimeSpans != 1 {
		t.Fatalf("runtime attempt spans = %d, want 1", runtimeSpans)
	}
	if routeSpan.Status().Code != codes.Ok {
		t.Fatalf("route span status = %v, want Ok", routeSpan.Status())
	}
	for key, want := range map[string]any{
		"route.selected.provider": "fake",
		"route.selected.model":    "primary",
		"route.executed.model":    "backup",
		"route.fallbacks":         1,
	} {
		attr, ok := routeSpanAttr(routeSpan, key)
		if !ok {
			t.Fatalf("missing route span attr %q", key)
		}
		var got any
		switch want.(type) {
		case string:
			got = attr.AsString()
		case int:
			got = attr.AsInt64()
		}
		if got != want {
			if gotInt, isInt := got.(int64); isInt && gotInt == int64(want.(int)) {
				continue
			}
			t.Fatalf("attr %q = %v, want %v", key, got, want)
		}
	}
	// Primary preflight failed (fallback-eligible), backup preflight
	// succeeded, backup execute succeeded = 3 attempt events.
	if got := len(routeSpan.Events()); got != 3 {
		t.Fatalf("attempt events = %d, want 3", got)
	}
	failed := 0
	for _, event := range routeSpan.Events() {
		if event.Name != "route.attempt" {
			t.Fatalf("unexpected event %q", event.Name)
		}
		attrs := map[string]attribute.Value{}
		for _, attr := range event.Attributes {
			attrs[string(attr.Key)] = attr.Value
		}
		if attrs["outcome"].AsString() == string(AttemptOutcomeFailed) {
			failed++
			if got := attrs["error_kind"].AsString(); got != string(inference.UnsupportedFeature) {
				t.Fatalf("failed attempt error_kind = %q", got)
			}
			if got := attrs["target.model"].AsString(); got != "primary" {
				t.Fatalf("failed attempt target = %q, want primary", got)
			}
			if got := attrs["phase"].AsString(); got != string(AttemptPhasePreflight) {
				t.Fatalf("failed attempt phase = %q, want preflight", got)
			}
		}
	}
	if failed != 1 {
		t.Fatalf("failed attempt events = %d, want 1", failed)
	}
}
