package setup

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/GizClaw/flowcraft/core/telemetry"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type options struct {
	export         sdktrace.SpanExporter
	serviceName    string
	serviceVersion string
	propagator     propagation.TextMapPropagator

	// optErr captures errors raised by TraceOption implementations
	// that build their own exporter (e.g. WithOTLPTraceExporter).
	// The zero-arg option signature does not allow returning an
	// error directly, so we stash it here and surface it from
	// InitTracer.
	optErr error
}

// TraceOption configures InitTracer behaviour.
type TraceOption func(*options)

func WithExporter(exp sdktrace.SpanExporter) TraceOption {
	return func(opts *options) {
		opts.export = exp
	}
}

func WithServiceName(name string) TraceOption {
	return func(opts *options) {
		opts.serviceName = name
	}
}

func WithServiceVersion(version string) TraceOption {
	return func(opts *options) {
		opts.serviceVersion = version
	}
}

// WithPropagator replaces the process-wide TextMapPropagator InitTracer
// installs. The default is W3C TraceContext plus Baggage, which is what
// every OTLP-compatible peer expects; override it only when a host must
// interoperate with a non-W3C trace system.
func WithPropagator(p propagation.TextMapPropagator) TraceOption {
	return func(opts *options) {
		opts.propagator = p
	}
}

// defaultPropagator is the W3C pair: TraceContext carries traceparent /
// tracestate, Baggage carries the cross-service key-values an operator
// attaches. The OTel global default is a no-op, so without this every
// Inject call silently does nothing and the trace stops at the process
// boundary.
func defaultPropagator() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
}

// InitTracer initializes the OpenTelemetry TracerProvider and the global
// TextMapPropagator.
//
// With an Exporter the provider uses WithBatcher for async export.
// Without one it installs a real SDK provider backed by discardExporter
// (via WithSyncer to avoid background goroutine overhead) so that valid
// trace/span IDs are still generated for structured log correlation.
//
// The propagator is installed here rather than in a separate call because
// tracing without propagation produces root spans on both sides of every
// process boundary — a trace that looks fine and joins nothing. A host
// that never calls InitTracer keeps the OTel no-op default, which is the
// correct behaviour for a library.
func InitTracer(ctx context.Context, opts ...TraceOption) (func(context.Context) error, error) {
	o := &options{
		serviceName:    telemetry.ServiceName,
		serviceVersion: telemetry.ServiceVersion,
	}
	for _, fn := range opts {
		fn(o)
	}
	if o.optErr != nil {
		return nil, o.optErr
	}

	userProvidedExporter := o.export != nil
	if o.export == nil {
		o.export = discardExporter{}
	}

	res, err := buildResource(ctx, o.serviceName, o.serviceVersion)
	if err != nil {
		return nil, fmt.Errorf("telemetry: create trace resource: %w", err)
	}

	tpOpts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
	}
	if userProvidedExporter {
		tpOpts = append(tpOpts, sdktrace.WithBatcher(o.export))
	} else {
		tpOpts = append(tpOpts, sdktrace.WithSyncer(o.export))
	}

	tp := sdktrace.NewTracerProvider(tpOpts...)
	otel.SetTracerProvider(tp)

	propagator := o.propagator
	if propagator == nil {
		propagator = defaultPropagator()
	}
	otel.SetTextMapPropagator(propagator)

	return tp.Shutdown, nil
}

// discardExporter drops all exported spans. Used when no real exporter is
// configured so that the SDK still generates valid trace/span IDs.
type discardExporter struct{}

func (discardExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error { return nil }
func (discardExporter) Shutdown(context.Context) error                             { return nil }
