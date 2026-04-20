package bootstrap

import (
	"context"
	"net/url"
	"os"
	"path/filepath"

	"github.com/GizClaw/flowcraft/internal/config"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdkx/telemetry/logfile"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

func initTelemetry(ctx context.Context, cfg *config.Config) (func(context.Context) error, error) {
	var opts []telemetry.Option

	if cfg.Telemetry.Enabled && cfg.Telemetry.Endpoint != "" {
		buildOTLPOpts(cfg, ctx, &opts)
	}

	logProcs := buildLogProcessors(ctx, cfg)
	// Suppress the SDK's default-on console sink (v0.1.x compatibility
	// shim) so it doesn't double up with the explicit ConsoleProcessors
	// included in logProcs. Drop the WithLogConsole call once the SDK
	// removes the default in v0.2.0.
	logOpts := make([]telemetry.LogOption, 0, len(logProcs)+1)
	logOpts = append(logOpts, telemetry.WithLogConsole(false))
	for _, p := range logProcs {
		logOpts = append(logOpts, telemetry.WithLogProcessor(p))
	}
	opts = append(opts, telemetry.LoggerOpts(logOpts...))

	shutdown, err := telemetry.InitAll(ctx, opts...)
	if err != nil {
		return nil, err
	}

	telemetry.Info(ctx, "telemetry initialized",
		otellog.Bool("enabled", cfg.Telemetry.Enabled),
		otellog.String("endpoint", redactEndpointForLog(cfg.Telemetry.Endpoint)),
		otellog.String("log.file", cfg.LogFilePath()))

	return shutdown, nil
}

// buildLogProcessors composes the server's log sinks: the canonical
// console split (stdout for INFO..<WARN, stderr for WARN..) plus a
// rotating local file when configured. The min severity comes from
// cfg.Log.Level.
func buildLogProcessors(ctx context.Context, cfg *config.Config) []sdklog.Processor {
	min := logSeverityFromConfig(cfg.Log.Level)
	procs := telemetry.ConsoleProcessors(min)

	logPath := cfg.LogFilePath()
	if logPath == "" {
		return procs
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		telemetry.Error(ctx, "telemetry: failed to create log directory",
			otellog.String("path", logPath),
			otellog.String("error", err.Error()))
		return procs
	}
	exp, err := logfile.NewExporter(logfile.Config{
		Path:       logPath,
		MaxSizeMB:  cfg.Log.File.MaxSizeMB,
		MaxBackups: cfg.Log.File.MaxBackups,
		MaxAgeDays: cfg.Log.File.MaxAgeDays,
		Compress:   cfg.Log.File.Compress,
	})
	if err != nil {
		telemetry.Error(ctx, "telemetry: failed to create log file exporter",
			otellog.String("path", logPath),
			otellog.String("error", err.Error()))
		return procs
	}
	fileProc := telemetry.NewSeverityFilter(
		sdklog.NewBatchProcessor(exp),
		min, 0,
	)
	return append(procs, fileProc)
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
		telemetry.Error(ctx, "telemetry: failed to create OTLP trace exporter", otellog.String("error", err.Error()))
	} else {
		*opts = append(*opts, telemetry.TracerOpts(telemetry.WithExporter(traceExp)))
	}

	if meterExp, err := otlpmetrichttp.New(ctx, meterExpOpts...); err != nil {
		telemetry.Error(ctx, "telemetry: failed to create OTLP metric exporter", otellog.String("error", err.Error()))
	} else {
		*opts = append(*opts, telemetry.MeterOpts(telemetry.WithMeterExporter(meterExp)))
	}

	if logExp, err := otlploghttp.New(ctx, logExpOpts...); err != nil {
		telemetry.Error(ctx, "telemetry: failed to create OTLP log exporter", otellog.String("error", err.Error()))
	} else {
		min := logSeverityFromConfig(cfg.Log.Level)
		proc := telemetry.NewSeverityFilter(
			sdklog.NewBatchProcessor(logExp),
			min, 0,
		)
		*opts = append(*opts, telemetry.LoggerOpts(telemetry.WithLogProcessor(proc)))
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
