package telemetry

import (
	"context"
	"testing"
)

func TestTracerOpts(t *testing.T) {
	cfg := &initAllOpts{}
	opt := TracerOpts(WithServiceName("svc"))
	opt(cfg)
	if len(cfg.trace) != 1 {
		t.Fatalf("expected 1 trace option, got %d", len(cfg.trace))
	}
}

func TestMeterOpts(t *testing.T) {
	cfg := &initAllOpts{}
	opt := MeterOpts(WithMeterServiceName("svc"))
	opt(cfg)
	if len(cfg.meter) != 1 {
		t.Fatalf("expected 1 meter option, got %d", len(cfg.meter))
	}
}

func TestLoggerOpts(t *testing.T) {
	cfg := &initAllOpts{}
	opt := LoggerOpts(WithLogServiceName("svc"))
	opt(cfg)
	if len(cfg.log) != 1 {
		t.Fatalf("expected 1 log option, got %d", len(cfg.log))
	}
}

func TestInitAll_DefaultOptions(t *testing.T) {
	ctx := context.Background()
	shutdown, err := InitAll(ctx)
	if err != nil {
		t.Fatalf("InitAll error: %v", err)
	}

	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown error: %v", err)
	}
}

func TestInitAll_NilOptionIgnored(t *testing.T) {
	ctx := context.Background()
	shutdown, err := InitAll(ctx, nil)
	if err != nil {
		t.Fatalf("InitAll error: %v", err)
	}
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown error: %v", err)
	}
}

func TestInitAll_WithAllSubOptions(t *testing.T) {
	ctx := context.Background()
	shutdown, err := InitAll(ctx,
		TracerOpts(WithServiceName("test"), WithServiceVersion("1.0")),
		MeterOpts(WithMeterServiceName("test"), WithMeterServiceVersion("1.0")),
		LoggerOpts(WithLogServiceName("test"), WithLogServiceVersion("1.0")),
	)
	if err != nil {
		t.Fatalf("InitAll error: %v", err)
	}
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown error: %v", err)
	}
}
