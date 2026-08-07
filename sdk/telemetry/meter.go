package telemetry

import (
	"context"
	"fmt"
	"time"

	otelruntime "go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

type meterOptions struct {
	export         sdkmetric.Exporter
	reader         sdkmetric.Reader
	serviceName    string
	serviceVersion string

	runtimeMetrics         bool
	runtimeMetricsInterval time.Duration

	// optErr — see options.optErr in trace.go.
	optErr error
}

// MeterOption configures InitMeter behaviour.
type MeterOption func(*meterOptions)

func WithMeterExporter(exp sdkmetric.Exporter) MeterOption {
	return func(opts *meterOptions) {
		opts.export = exp
	}
}

// WithMeterReader replaces the default PeriodicReader with an explicit
// [sdkmetric.Reader]. Use it when you need deterministic collection in tests
// (e.g. [sdkmetric.NewManualReader]) or want to attach a runtime metric
// producer. It is mutually exclusive with [WithMeterExporter].
func WithMeterReader(r sdkmetric.Reader) MeterOption {
	return func(opts *meterOptions) {
		opts.reader = r
	}
}

func WithMeterServiceName(name string) MeterOption {
	return func(opts *meterOptions) {
		opts.serviceName = name
	}
}

func WithMeterServiceVersion(version string) MeterOption {
	return func(opts *meterOptions) {
		opts.serviceVersion = version
	}
}

// WithRuntimeMetrics enables Go runtime metrics collection (memory, GC,
// goroutines, GOMAXPROCS) via
// go.opentelemetry.io/contrib/instrumentation/runtime. The interval is the
// minimum time between reads of the Go runtime metrics; values <= 0 fall back
// to the contrib default of 15s. Metrics are exported through the same
// MeterProvider configured for this pipeline, so combine it with
// [WithMeterExporter] or [WithMeterReader] when you want them to reach a
// collector.
func WithRuntimeMetrics(interval time.Duration) MeterOption {
	return func(opts *meterOptions) {
		opts.runtimeMetrics = true
		opts.runtimeMetricsInterval = interval
	}
}

// InitMeter initializes the OpenTelemetry MeterProvider.
//
// With an Exporter it creates a PeriodicReader for regular metric collection.
// Without one the provider is created with no reader (noop — instruments are
// valid but never exported).
func InitMeter(ctx context.Context, opts ...MeterOption) (func(context.Context) error, error) {
	o := &meterOptions{
		serviceName:    ServiceName,
		serviceVersion: ServiceVersion,
	}
	for _, fn := range opts {
		fn(o)
	}
	if o.optErr != nil {
		return nil, o.optErr
	}
	if o.reader != nil && o.export != nil {
		return nil, fmt.Errorf("telemetry: WithMeterReader and WithMeterExporter are mutually exclusive")
	}

	res, err := buildResource(ctx, o.serviceName, o.serviceVersion)
	if err != nil {
		return nil, fmt.Errorf("telemetry: create metric resource: %w", err)
	}

	var mp *sdkmetric.MeterProvider
	switch {
	case o.reader != nil:
		mp = sdkmetric.NewMeterProvider(
			sdkmetric.WithResource(res),
			sdkmetric.WithReader(o.reader),
		)
	case o.export != nil:
		reader := sdkmetric.NewPeriodicReader(o.export)
		mp = sdkmetric.NewMeterProvider(
			sdkmetric.WithResource(res),
			sdkmetric.WithReader(reader),
		)
	default:
		mp = sdkmetric.NewMeterProvider(
			sdkmetric.WithResource(res),
		)
	}

	otel.SetMeterProvider(mp)
	if o.runtimeMetrics {
		if err := otelruntime.Start(
			otelruntime.WithMinimumReadMemStatsInterval(o.runtimeMetricsInterval),
		); err != nil {
			_ = mp.Shutdown(ctx)
			return nil, fmt.Errorf("telemetry: start runtime metrics: %w", err)
		}
	}
	return mp.Shutdown, nil
}
