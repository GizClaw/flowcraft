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

func initTelemetry(ctx context.Context, cfg *config.Config) (func(context.Context) error, error) {
	var opts []telemetry.Option

	if cfg.Telemetry.Enabled && cfg.Telemetry.Endpoint != "" {
		buildOTLPOpts(cfg, ctx, &opts)
	}

	opts = append(opts, telemetry.LoggerOpts(
		telemetry.WithLogConsole(true),
		telemetry.WithLogMinSeverity(logSeverityFromConfig(cfg.Log.Level)),
	))

	shutdown, err := telemetry.InitAll(ctx, opts...)
	if err != nil {
		return nil, err
	}

	telemetry.Info(ctx, "telemetry initialized",
		otellog.Bool("enabled", cfg.Telemetry.Enabled),
		otellog.String("endpoint", redactEndpointForLog(cfg.Telemetry.Endpoint)))

	return shutdown, nil
}

func buildOTLPOpts(cfg *config.Config, ctx context.Context, opts *[]telemetry.Option) {
	endpoint := cfg.Telemetry.Endpoint

	traceExpOpts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(endpoint)}
	meterExpOpts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpoint(endpoint)}
	logExpOpts := []otlploghttp.Option{otlploghttp.WithEndpoint(endpoint)}
	if cfg.Telemetry.Insecure {
		traceExpOpts = append(traceExpOpts, otlptracehttp.WithInsecure())
		meterExpOpts = append(meterExpOpts, otlpmetrichttp.WithInsecure())
		logExpOpts = append(logExpOpts, otlploghttp.WithInsecure())
	}

	if traceExp, err := otlptracehttp.New(ctx, traceExpOpts...); err != nil {
		slog.Error("telemetry: failed to create OTLP trace exporter", "error", err)
	} else {
		*opts = append(*opts, telemetry.TracerOpts(telemetry.WithExporter(traceExp)))
	}

	if meterExp, err := otlpmetrichttp.New(ctx, meterExpOpts...); err != nil {
		slog.Error("telemetry: failed to create OTLP metric exporter", "error", err)
	} else {
		*opts = append(*opts, telemetry.MeterOpts(telemetry.WithMeterExporter(meterExp)))
	}

	if logExp, err := otlploghttp.New(ctx, logExpOpts...); err != nil {
		slog.Error("telemetry: failed to create OTLP log exporter", "error", err)
	} else {
		*opts = append(*opts, telemetry.LoggerOpts(telemetry.WithLogExporter(logExp)))
	}
}

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
