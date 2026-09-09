package middleware

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool"
)

func TestTelemetry_EmitsExecutionSpan(t *testing.T) {
	rec := installTelemetryTestTracer(t)
	exec := tool.NewExecutor(catalogWith(echoTool("echo")), Telemetry())
	tc := call("echo")

	res := exec.Execute(context.Background(), tc)
	if res.IsError {
		t.Fatalf("execute = %q, want success", res.Content)
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Name() != "echo.execute" {
		t.Errorf("span name = %q, want %q", span.Name(), "echo.execute")
	}
	if span.Status().Code != codes.Ok {
		t.Errorf("span status = %v, want Ok", span.Status().Code)
	}
	attrs := telemetrySpanAttrs(span)
	if attrs[telemetry.AttrToolName] != "echo" ||
		attrs[telemetry.AttrToolCallID] != tc.ID {
		t.Errorf("span attrs = %#v, want tool name/call id", attrs)
	}
}

func TestFromSettings_EnablesTelemetry(t *testing.T) {
	rec := installTelemetryTestTracer(t)
	mws, err := FromSettings(Settings{
		Telemetry: &TelemetrySettings{Enabled: true},
	})
	if err != nil {
		t.Fatalf("FromSettings: %v", err)
	}
	if len(mws) != 1 {
		t.Fatalf("middlewares = %d, want 1", len(mws))
	}

	exec := tool.NewExecutor(catalogWith(echoTool("echo")), mws...)
	if res := exec.Execute(context.Background(), call("echo")); res.IsError {
		t.Fatalf("execute = %q, want success", res.Content)
	}
	if spans := rec.Ended(); len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
}

func TestFromSettings_SkipsDisabledOrAbsentTelemetry(t *testing.T) {
	for _, settings := range []Settings{
		{},
		{Telemetry: &TelemetrySettings{}},
		{Telemetry: &TelemetrySettings{Enabled: false}},
	} {
		mws, err := FromSettings(settings)
		if err != nil {
			t.Fatalf("FromSettings(%+v): %v", settings, err)
		}
		if len(mws) != 0 {
			t.Errorf("FromSettings(%+v) returned %d middlewares, want 0", settings, len(mws))
		}
	}
}

// installTelemetryTestTracer installs an in-memory TracerProvider and
// restores the previous global provider on cleanup.
func installTelemetryTestTracer(t *testing.T) *tracetest.SpanRecorder {
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

func telemetrySpanAttrs(span sdktrace.ReadOnlySpan) map[string]any {
	attrs := make(map[string]any, len(span.Attributes()))
	for _, kv := range span.Attributes() {
		attrs[string(kv.Key)] = kv.Value.AsInterface()
	}
	return attrs
}
