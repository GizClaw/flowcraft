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

	shutdownTelemetry, err := initTelemetry(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}

	// Emit config diagnostics *after* telemetry is up so they reach the
	// configured sinks; before this point telemetry calls are dropped.
	for _, w := range cfg.Validate() {
		telemetry.Warn(ctx, "config validation", otellog.String("warning", w))
	}
	if config.HomeRootDegraded() {
		// HOME is unset (most commonly a systemd unit without
		// Environment=HOME=...). Anything derived from HomeRoot now lives
		// on /tmp and will not survive reboot. Server data (DB, workspace,
		// checkpoints, logs) is still safe because it is pinned via
		// DataDir / FLOWCRAFT_DATA_DIR.
		telemetry.Warn(ctx, "config: HomeRoot degraded — UserHomeDir failed, using tmpfs fallback; HomeRoot-derived paths will not persist across reboot",
			otellog.String("home_root", config.HomeRoot()))
	}
	telemetry.Info(ctx, "config loaded",
		otellog.String("address", cfg.Address()),
		otellog.String("home_root", config.HomeRoot()),
		otellog.String("data_dir", config.DataDir()))
	cleanup := func() {
		tctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTelemetry(tctx)
	}

	return cfg, cleanup, nil
}
