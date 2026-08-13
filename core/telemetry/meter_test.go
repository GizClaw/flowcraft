package telemetry

import (
	"context"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestWithRuntimeMetrics(t *testing.T) {
	o := &meterOptions{}
	WithRuntimeMetrics(3 * time.Second)(o)
	if !o.runtimeMetrics {
		t.Fatal("expected runtime metrics to be enabled")
	}
	if o.runtimeMetricsInterval != 3*time.Second {
		t.Fatalf("expected interval 3s, got %v", o.runtimeMetricsInterval)
	}
}

func TestWithMeterReader(t *testing.T) {
	o := &meterOptions{}
	r := sdkmetric.NewManualReader()
	WithMeterReader(r)(o)
	if o.reader != r {
		t.Fatal("expected provided reader to be stored")
	}
}

func TestInitMeter_WithMeterReader_UsesProvidedReader(t *testing.T) {
	ctx := context.Background()
	reader := sdkmetric.NewManualReader()
	shutdown, err := InitMeter(ctx, WithMeterReader(reader))
	if err != nil {
		t.Fatalf("InitMeter error: %v", err)
	}
	defer func() { _ = shutdown(ctx) }()

	counter, err := Meter().Int64Counter("test.counter")
	if err != nil {
		t.Fatalf("create counter error: %v", err)
	}
	counter.Add(ctx, 1)

	var resources metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &resources); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !hasMetric(resources, "test.counter") {
		t.Fatal("expected test.counter to be exported via the provided reader")
	}
}

func TestInitMeter_ReaderAndExporterConflict(t *testing.T) {
	_, err := InitMeter(context.Background(),
		WithMeterReader(sdkmetric.NewManualReader()),
		WithMeterExporter(noopMeterExporter{}),
	)
	if err == nil {
		t.Fatal("expected error for conflicting reader and exporter")
	}
}

func TestInitMeter_WithRuntimeMetrics_ExportsGoRuntimeMetrics(t *testing.T) {
	ctx := context.Background()
	reader := sdkmetric.NewManualReader()
	shutdown, err := InitMeter(ctx,
		WithMeterReader(reader),
		WithRuntimeMetrics(time.Second),
	)
	if err != nil {
		t.Fatalf("InitMeter error: %v", err)
	}
	defer func() { _ = shutdown(ctx) }()

	var resources metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &resources); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	for _, name := range []string{
		"go.memory.used",
		"go.memory.allocated",
		"go.goroutine.count",
	} {
		if !hasMetric(resources, name) {
			t.Errorf("runtime metric %q was not exported", name)
		}
	}
}

func hasMetric(resources metricdata.ResourceMetrics, name string) bool {
	for _, scope := range resources.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == name {
				return true
			}
		}
	}
	return false
}

type noopMeterExporter struct{}

func (noopMeterExporter) Temporality(sdkmetric.InstrumentKind) metricdata.Temporality {
	return metricdata.CumulativeTemporality
}

func (noopMeterExporter) Aggregation(sdkmetric.InstrumentKind) sdkmetric.Aggregation {
	return nil
}

func (noopMeterExporter) Export(context.Context, *metricdata.ResourceMetrics) error {
	return nil
}

func (noopMeterExporter) ForceFlush(context.Context) error {
	return nil
}

func (noopMeterExporter) Shutdown(context.Context) error {
	return nil
}
