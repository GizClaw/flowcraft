package bootstrap

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/internal/config"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	otellog "go.opentelemetry.io/otel/log"
)

// wireConfig ensures directory layout, loads and validates config, and
// initialises the telemetry pipeline. The returned cleanup shuts down
// the OTLP exporters.
func wireConfig(ctx context.Context) (*config.Config, func(), error) {
	if err := config.EnsureLayout(); err != nil {
		return nil, nil, err
	}

	cfg := config.Load()
	config.InitLogging(cfg.Log)
	for _, w := range cfg.Validate() {
		telemetry.Warn(ctx, "config validation", otellog.String("warning", w))
	}
	telemetry.Info(ctx, "config loaded",
		otellog.String("address", cfg.Address()),
		otellog.String("configure_path", cfg.ConfigurePath))

	shutdownTelemetry, err := initTelemetry(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() {
		tctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTelemetry(tctx)
	}

	return cfg, cleanup, nil
}
