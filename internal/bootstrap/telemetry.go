package bootstrap

import (
	"context"
	"log/slog"
	"net/url"

	"github.com/GizClaw/flowcraft/internal/config"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otellog "go.opentelemetry.io/otel/log"
)

func initTelemetry(ctx context.Context, cfg *config.Config) (
	shutdownTracer func(context.Context) error,
	shutdownMeter func(context.Context) error,
	shutdownLogPipeline func(context.Context) error,
) {
	noop := func(context.Context) error { return nil }

	var traceOpts []telemetry.TraceOption
	if cfg.Telemetry.Enabled && cfg.Telemetry.Endpoint != "" {
		exporterOpts := []otlptracehttp.Option{
			otlptracehttp.WithEndpoint(cfg.Telemetry.Endpoint),
		}
		if cfg.Telemetry.Insecure {
			exporterOpts = append(exporterOpts, otlptracehttp.WithInsecure())
		}
		exp, err := otlptracehttp.New(ctx, exporterOpts...)
		if err != nil {
			slog.Error("telemetry: failed to create OTLP trace exporter", "error", err)
		} else {
			traceOpts = append(traceOpts, telemetry.WithExporter(exp))
		}
	}
	st, err := telemetry.InitTracer(ctx, traceOpts...)
	if err != nil {
		slog.Error("telemetry: failed to init tracer", "error", err)
		shutdownTracer = noop
	} else {
		shutdownTracer = st
	}

	var meterOpts []telemetry.MeterOption
	if cfg.Telemetry.Enabled && cfg.Telemetry.Endpoint != "" {
		exporterOpts := []otlpmetrichttp.Option{
			otlpmetrichttp.WithEndpoint(cfg.Telemetry.Endpoint),
		}
		if cfg.Telemetry.Insecure {
			exporterOpts = append(exporterOpts, otlpmetrichttp.WithInsecure())
		}
		exp, err := otlpmetrichttp.New(ctx, exporterOpts...)
		if err != nil {
			slog.Error("telemetry: failed to create OTLP metric exporter", "error", err)
		} else {
			meterOpts = append(meterOpts, telemetry.WithMeterExporter(exp))
		}
	}
	sm, err := telemetry.InitMeter(ctx, meterOpts...)
	if err != nil {
		slog.Error("telemetry: failed to init meter", "error", err)
		shutdownMeter = noop
	} else {
		shutdownMeter = sm
	}

	logMinSev := logSeverityFromConfig(cfg.Log.Level)
	logOpts := []telemetry.LogOption{
		telemetry.WithLogConsole(true),
		telemetry.WithLogMinSeverity(logMinSev),
	}
	if cfg.Telemetry.Enabled && cfg.Telemetry.Endpoint != "" {
		exporterOpts := []otlploghttp.Option{
			otlploghttp.WithEndpoint(cfg.Telemetry.Endpoint),
		}
		if cfg.Telemetry.Insecure {
			exporterOpts = append(exporterOpts, otlploghttp.WithInsecure())
		}
		exp, err := otlploghttp.New(ctx, exporterOpts...)
		if err != nil {
			slog.Error("telemetry: failed to create OTLP log exporter", "error", err)
		} else {
			logOpts = append(logOpts, telemetry.WithLogExporter(exp))
		}
	}
	sl, err := telemetry.InitLog(ctx, logOpts...)
	if err != nil {
		slog.Error("telemetry: failed to init log pipeline", "error", err)
		shutdownLogPipeline = noop
	} else {
		shutdownLogPipeline = sl
	}

	telemetry.Info(ctx, "telemetry initialized",
		otellog.Bool("enabled", cfg.Telemetry.Enabled),
		otellog.String("endpoint", redactEndpointForLog(cfg.Telemetry.Endpoint)))
	return
}

// redactEndpointForLog strips userinfo and query from OTLP endpoints before logging.
func redactEndpointForLog(endpoint string) string {
	if endpoint == "" {
		return ""
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		if len(endpoint) > 96 {
			return endpoint[:96] + "…"
		}
		return endpoint
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	out := u.String()
	if len(out) > 256 {
		return out[:256] + "…"
	}
	return out
}

func logSeverityFromConfig(level string) otellog.Severity {
	switch level {
	case "debug":
		return otellog.SeverityDebug
	case "warn":
		return otellog.SeverityWarn
	case "error":
		return otellog.SeverityError
	default:
		return otellog.SeverityInfo
	}
}
