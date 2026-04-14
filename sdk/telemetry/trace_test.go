package telemetry

import (
	"context"
	"testing"
)

func TestInitTracer_NoExporter_StillCreatesValidIDs(t *testing.T) {
	ctx := context.Background()
	shutdown, err := InitTracer(ctx)
	if err != nil {
		t.Fatalf("InitTracer error: %v", err)
	}

	_, span := Tracer().Start(ctx, "test")
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

	m := Meter()
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
