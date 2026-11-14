package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
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

// These exercise the instrumentation half with no provider installed, which is the
// state every library consumer is in: a package that logs must be safe before the
// host wired its exporters. Provider installation is covered in telemetry/setup.
func TestEmitAllSeverities(t *testing.T) {
	ctx := context.Background()
	Enable()

	Trace(ctx, "trace msg")
	Debug(ctx, "debug msg")
	Info(ctx, "info msg")
	Warn(ctx, "warn msg")
	Error(ctx, "error msg")
}

func TestEmitWithNilContext(t *testing.T) {
	Enable()
	//nolint:staticcheck // deliberate: verify nil Context does not panic
	Info(nil, "nil context msg")
}

func TestEmitWithAttributes(t *testing.T) {
	ctx := context.Background()
	Enable()
	Info(ctx, "msg with attrs", attribute.String("key", "value"), attribute.Int("n", 42))
}

func TestEmitWithTraceContext(t *testing.T) {
	ctx := context.Background()

	Enable()
	ctx, span := Tracer().Start(ctx, "test-span")
	Info(ctx, "with trace context")
	span.End()
}
