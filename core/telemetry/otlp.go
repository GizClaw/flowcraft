package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// OTLPConfig is the small wire-protocol-agnostic surface shared by
// the three WithOTLP* shortcuts in this file. It captures the 90%
// case (HTTP, optional headers, optional auth) without exposing
// the full transport-specific option zoo of the underlying OTel
// exporter packages — callers that need finer control should use
// [WithExporter] / [WithMeterExporter] / [WithLogProcessor]
// directly with a self-built exporter.
type OTLPConfig struct {
	// Endpoint is the OTLP collector endpoint host[:port].
	//
	// Examples:
	//   - "otel-collector:4318"             (HTTP, default port)
	//   - "api.honeycomb.io"                 (managed, TLS)
	//   - "localhost:4318"                   (local dev)
	//
	// MUST NOT include a scheme — Insecure controls TLS. Path
	// suffixes are supported by the underlying exporter via
	// URLPath below.
	Endpoint string

	// URLPath optionally overrides the default OTLP HTTP path
	// (/v1/traces, /v1/metrics, /v1/logs). Empty = use defaults.
	URLPath string

	// Headers are sent on every export request. Use for managed
	// SaaS auth (e.g. Honeycomb's "x-honeycomb-team", Grafana
	// Cloud basic auth). Keys are case-insensitive per HTTP.
	Headers map[string]string

	// Insecure disables TLS. Use only for in-cluster collectors.
	// Default false (TLS on).
	Insecure bool
}

// WithOTLPTraceExporter is the lazy alternative to manually
// constructing an [otlptracehttp.New] exporter and feeding it to
// [WithExporter]. It returns a [TraceOption] that installs an
// OTLP/HTTP trace exporter pointing at the configured endpoint.
//
// Use it via [InitTracer] or [InitAll]:
//
//	shutdown, err := telemetry.InitAll(ctx,
//	    telemetry.TracerOpts(
//	        telemetry.WithOTLPTraceExporter(telemetry.OTLPConfig{
//	            Endpoint: "otel-collector:4318",
//	            Insecure: true,
//	        }),
//	    ),
//	)
//
// Construction errors (bad endpoint format, etc.) surface from
// [InitTracer] rather than from this helper.
func WithOTLPTraceExporter(cfg OTLPConfig) TraceOption {
	return func(o *options) {
		opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(cfg.Endpoint)}
		if cfg.URLPath != "" {
			opts = append(opts, otlptracehttp.WithURLPath(cfg.URLPath))
		}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlptracehttp.WithHeaders(cfg.Headers))
		}
		if cfg.Insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		// Construction blocks only briefly: it parses options and
		// builds an http.Client; no wire I/O happens until
		// ExportSpans is called by the SDK.
		exp, err := otlptracehttp.New(context.Background(), opts...)
		if err != nil {
			o.optErr = fmt.Errorf("WithOTLPTraceExporter: %w", err)
			return
		}
		o.export = exp
	}
}

// WithOTLPMeterExporter is the metric-side counterpart of
// [WithOTLPTraceExporter]. Sends OTLP/HTTP metric exports to the
// configured collector.
func WithOTLPMeterExporter(cfg OTLPConfig) MeterOption {
	return func(o *meterOptions) {
		opts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpoint(cfg.Endpoint)}
		if cfg.URLPath != "" {
			opts = append(opts, otlpmetrichttp.WithURLPath(cfg.URLPath))
		}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlpmetrichttp.WithHeaders(cfg.Headers))
		}
		if cfg.Insecure {
			opts = append(opts, otlpmetrichttp.WithInsecure())
		}
		exp, err := otlpmetrichttp.New(context.Background(), opts...)
		if err != nil {
			o.optErr = fmt.Errorf("WithOTLPMeterExporter: %w", err)
			return
		}
		o.export = exp
	}
}

// WithOTLPLogProcessor is the log-side counterpart. Wraps an
// OTLP/HTTP log exporter in a batch processor and appends it to
// the LoggerProvider's processor list.
func WithOTLPLogProcessor(cfg OTLPConfig) LogOption {
	return func(o *logOptions) {
		opts := []otlploghttp.Option{otlploghttp.WithEndpoint(cfg.Endpoint)}
		if cfg.URLPath != "" {
			opts = append(opts, otlploghttp.WithURLPath(cfg.URLPath))
		}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlploghttp.WithHeaders(cfg.Headers))
		}
		if cfg.Insecure {
			opts = append(opts, otlploghttp.WithInsecure())
		}
		exp, err := otlploghttp.New(context.Background(), opts...)
		if err != nil {
			o.optErr = fmt.Errorf("WithOTLPLogProcessor: %w", err)
			return
		}
		// Symmetric with WithBatcher on the trace path.
		o.processors = append(o.processors, sdklog.NewBatchProcessor(exp))
	}
}
