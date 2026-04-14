package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type options struct {
	export         sdktrace.SpanExporter
	serviceName    string
	serviceVersion string
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

// InitTracer initializes the OpenTelemetry TracerProvider.
//
// With an Exporter the provider uses WithBatcher for async export.
// Without one it installs a real SDK provider backed by discardExporter
// (via WithSyncer to avoid background goroutine overhead) so that valid
// trace/span IDs are still generated for structured log correlation.
func InitTracer(ctx context.Context, opts ...TraceOption) (func(context.Context) error, error) {
	o := &options{
		serviceName:    ServiceName,
		serviceVersion: ServiceVersion,
	}
	for _, fn := range opts {
		fn(o)
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

	return tp.Shutdown, nil
}

// discardExporter drops all exported spans. Used when no real exporter is
// configured so that the SDK still generates valid trace/span IDs.
type discardExporter struct{}

func (discardExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error { return nil }
func (discardExporter) Shutdown(context.Context) error                             { return nil }
