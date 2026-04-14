package telemetry

import (
	"context"
	"testing"

	otellog "go.opentelemetry.io/otel/log"
)

func TestSetLoggerName(t *testing.T) {
	SetLoggerName("custom/scope")
	v := loggerName.Load()
	name, _ := v.(string)
	if name != "custom/scope" {
		t.Fatalf("expected 'custom/scope', got %q", name)
	}
	SetLoggerName("")
}

func TestEmitAllSeverities(t *testing.T) {
	ctx := context.Background()
	shutdown, err := InitLog(ctx, WithLogConsole(false))
	if err != nil {
		t.Fatalf("InitLog error: %v", err)
	}
	defer func() { _ = shutdown(ctx) }()

	Enable()

	Trace(ctx, "trace msg")
	Debug(ctx, "debug msg")
	Info(ctx, "info msg")
	Warn(ctx, "warn msg")
	Error(ctx, "error msg")
}

func TestEmitWithNilContext(t *testing.T) {
	shutdown, err := InitLog(context.Background(), WithLogConsole(false))
	if err != nil {
		t.Fatalf("InitLog error: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	Enable()
	Info(nil, "nil context msg")
}

func TestEmitWithAttributes(t *testing.T) {
	ctx := context.Background()
	shutdown, err := InitLog(ctx, WithLogConsole(false))
	if err != nil {
		t.Fatalf("InitLog error: %v", err)
	}
	defer func() { _ = shutdown(ctx) }()

	Enable()
	Info(ctx, "msg with attrs", otellog.String("key", "value"), otellog.Int("n", 42))
}

func TestEmitWithTraceContext(t *testing.T) {
	ctx := context.Background()

	traceShutdown, err := InitTracer(ctx)
	if err != nil {
		t.Fatalf("InitTracer error: %v", err)
	}
	defer func() { _ = traceShutdown(ctx) }()

	logShutdown, err := InitLog(ctx, WithLogConsole(false))
	if err != nil {
		t.Fatalf("InitLog error: %v", err)
	}
	defer func() { _ = logShutdown(ctx) }()

	Enable()
	ctx, span := Tracer().Start(ctx, "test-span")
	Info(ctx, "with trace context")
	span.End()
}
