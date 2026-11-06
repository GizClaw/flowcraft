package setup

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/GizClaw/flowcraft/core/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func TestInitTracer_NoExporter_StillCreatesValidIDs(t *testing.T) {
	ctx := context.Background()
	shutdown, err := InitTracer(ctx)
	if err != nil {
		t.Fatalf("InitTracer error: %v", err)
	}

	_, span := telemetry.Tracer().Start(ctx, "test")
	sc := span.SpanContext()
	if !sc.TraceID().IsValid() {
		t.Fatalf("expected valid trace id")
	}
	if !sc.SpanID().IsValid() {
		t.Fatalf("expected valid span id")
	}
	span.End()

	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown error: %v", err)
	}
}

func TestInitMeter_NoExporter_Succeeds(t *testing.T) {
	ctx := context.Background()
	shutdown, err := InitMeter(ctx)
	if err != nil {
		t.Fatalf("InitMeter error: %v", err)
	}

	m := telemetry.Meter()
	counter, err := m.Int64Counter("test.counter")
	if err != nil {
		t.Fatalf("create counter error: %v", err)
	}
	counter.Add(ctx, 1)

	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown error: %v", err)
	}
}

func TestWithExporter(t *testing.T) {
	o := &options{}
	WithExporter(nil)(o)
	if o.export != nil {
		t.Fatal("expected nil export")
	}
}

func TestWithServiceName(t *testing.T) {
	o := &options{}
	WithServiceName("svc")(o)
	if o.serviceName != "svc" {
		t.Fatalf("expected 'svc', got %q", o.serviceName)
	}
}

func TestWithServiceVersion(t *testing.T) {
	o := &options{}
	WithServiceVersion("2.0")(o)
	if o.serviceVersion != "2.0" {
		t.Fatalf("expected '2.0', got %q", o.serviceVersion)
	}
}

func TestInitTracer_WithCustomServiceName(t *testing.T) {
	ctx := context.Background()
	shutdown, err := InitTracer(ctx, WithServiceName("custom"), WithServiceVersion("1.0"))
	if err != nil {
		t.Fatalf("InitTracer error: %v", err)
	}
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown error: %v", err)
	}
}

func TestWithMeterExporter(t *testing.T) {
	o := &meterOptions{}
	WithMeterExporter(nil)(o)
	if o.export != nil {
		t.Fatal("expected nil export")
	}
}

func TestWithMeterServiceName(t *testing.T) {
	o := &meterOptions{}
	WithMeterServiceName("svc")(o)
	if o.serviceName != "svc" {
		t.Fatalf("expected 'svc', got %q", o.serviceName)
	}
}

func TestWithMeterServiceVersion(t *testing.T) {
	o := &meterOptions{}
	WithMeterServiceVersion("2.0")(o)
	if o.serviceVersion != "2.0" {
		t.Fatalf("expected '2.0', got %q", o.serviceVersion)
	}
}

func TestInitMeter_WithCustomServiceName(t *testing.T) {
	ctx := context.Background()
	shutdown, err := InitMeter(ctx, WithMeterServiceName("custom"), WithMeterServiceVersion("1.0"))
	if err != nil {
		t.Fatalf("InitMeter error: %v", err)
	}
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown error: %v", err)
	}
}

func TestWithPropagator(t *testing.T) {
	o := &options{}
	// A pointer is comparable; the default propagator is a composite of
	// propagators, and comparing interface values that hold one panics.
	custom := &stubPropagator{}
	WithPropagator(custom)(o)
	if o.propagator != custom {
		t.Fatal("expected the custom propagator to be stored")
	}
}

func TestInitTracer_InstallsW3CPropagator(t *testing.T) {
	ctx := context.Background()
	shutdown, err := InitTracer(ctx)
	if err != nil {
		t.Fatalf("InitTracer error: %v", err)
	}
	defer func() {
		if err := shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	}()

	fields := otel.GetTextMapPropagator().Fields()
	for _, want := range []string{"traceparent", "tracestate", "baggage"} {
		if !slices.Contains(fields, want) {
			t.Fatalf("propagator fields %v missing %q", fields, want)
		}
	}

	ctx, span := telemetry.Tracer().Start(ctx, "root")
	defer span.End()

	carrier := propagation.HeaderCarrier(http.Header{})
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	if carrier.Get("traceparent") == "" {
		t.Fatal("expected a traceparent header after Inject")
	}

	extracted := otel.GetTextMapPropagator().Extract(
		context.Background(), carrier)
	if got := trace.SpanContextFromContext(extracted).TraceID(); got != span.SpanContext().TraceID() {
		t.Fatalf("extracted trace id = %s, want %s", got, span.SpanContext().TraceID())
	}
}

// stubPropagator is a comparable TextMapPropagator for tests that need identity.
type stubPropagator struct{}

func (*stubPropagator) Inject(context.Context, propagation.TextMapCarrier) {}

func (*stubPropagator) Extract(ctx context.Context, _ propagation.TextMapCarrier) context.Context {
	return ctx
}

func (*stubPropagator) Fields() []string { return nil }
