package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

type meterOptions struct {
	export         sdkmetric.Exporter
	serviceName    string
	serviceVersion string
}

// MeterOption configures InitMeter behaviour.
type MeterOption func(*meterOptions)

func WithMeterExporter(exp sdkmetric.Exporter) MeterOption {
	return func(opts *meterOptions) {
		opts.export = exp
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

	res, err := buildResource(ctx, o.serviceName, o.serviceVersion)
	if err != nil {
		return nil, fmt.Errorf("telemetry: create metric resource: %w", err)
	}

	var mp *sdkmetric.MeterProvider
	if o.export != nil {
		reader := sdkmetric.NewPeriodicReader(o.export)
		mp = sdkmetric.NewMeterProvider(
			sdkmetric.WithResource(res),
			sdkmetric.WithReader(reader),
		)
	} else {
		mp = sdkmetric.NewMeterProvider(
			sdkmetric.WithResource(res),
		)
	}

	otel.SetMeterProvider(mp)
	return mp.Shutdown, nil
}
