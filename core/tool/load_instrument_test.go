package tool_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/telemetry/telemetrytest"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/tooltest"
)

// TestLazyLoadEmitsSpanOnce pins the miss-path rule: a deferred tool's
// materialisation is one span, and the calls that find it already loaded add
// nothing. A span per call would be noise; no span at all hides a cold start.
func TestLazyLoadEmitsSpanOnce(t *testing.T) {
	recorder := telemetrytest.New(t)

	loads := 0
	lazy := tooltest.LazyTool("deferred", func(context.Context) (tool.Tool, error) {
		loads++
		return tooltest.FuncTool("deferred", "", func(context.Context, string) (string, error) {
			return "ok", nil
		}), nil
	})
	registry, err := tool.NewRegistry([]tool.Source{tooltest.LazySource(lazy)})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Close() })

	proxy, ok := registry.Get("deferred")
	if !ok {
		t.Fatal("lazy tool not registered")
	}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := proxy.Execute(ctx, `{}`); err != nil {
			t.Fatalf("execute %d: %v", i, err)
		}
	}

	if loads != 1 {
		t.Fatalf("loads = %d, want 1", loads)
	}
	recorder.RequireCount(t, "tool.load", 1)
	span := recorder.RequireNamed(t, "tool.load")
	telemetrytest.RequireAttrString(t, span, telemetry.AttrToolName, "deferred")
}

// TestDeferredLoadMetricsFollowTheInstalledProvider pins why recordLoadMetrics
// resolves the meter per call instead of caching it in a package variable: the
// second provider, installed after a load already happened, must receive the next
// load's series. A cached instrument — or a cached meter — binds to the provider
// that existed at the first load and every later load reports there instead.
func TestDeferredLoadMetricsFollowTheInstalledProvider(t *testing.T) {
	early := installToolMeter(t)
	loadDeferred(t, "early")
	late := installToolMeter(t)
	loadDeferred(t, "late")

	requireSingleLoad(t, collectMetrics(t, late), "late")
	requireSingleLoad(t, collectMetrics(t, early), "early")
}

// collectMetrics drains one manual reader into a fresh snapshot.
func collectMetrics(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("collect: %v", err)
	}
	return collected
}

// requireSingleLoad asserts the reader saw exactly one deferred load, named name:
// one increment of loads.total carrying that tool and one duration record.
func requireSingleLoad(t *testing.T, collected metricdata.ResourceMetrics, name string) {
	t.Helper()
	sum, ok := metricNamed(t, collected, "loads.total").Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("loads.total is not an int64 sum")
	}
	if len(sum.DataPoints) != 1 {
		t.Fatalf("loads.total points = %d, want 1", len(sum.DataPoints))
	}
	point := sum.DataPoints[0]
	if point.Value != 1 {
		t.Errorf("loads.total = %d, want 1", point.Value)
	}
	if v, ok := point.Attributes.Value(attribute.Key(telemetry.AttrToolName)); !ok || v.AsString() != name {
		t.Errorf("loads.total %s = %v, want %s", telemetry.AttrToolName, v.AsString(), name)
	}
	if v, ok := point.Attributes.Value(attribute.Key("status")); !ok || v.AsString() != "success" {
		t.Errorf("loads.total status = %v, want success", v.AsString())
	}

	histogram, ok := metricNamed(t, collected, "load.duration.seconds").Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("load.duration.seconds is not a float64 histogram")
	}
	var recorded uint64
	for _, duration := range histogram.DataPoints {
		recorded += duration.Count
	}
	if recorded != 1 {
		t.Errorf("load.duration.seconds count = %d, want 1", recorded)
	}
}

// installToolMeter points the global meter provider at a fresh manual reader,
// restoring the previous one on cleanup.
func installToolMeter(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	previous := otel.GetMeterProvider()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetMeterProvider(previous)
	})
	return reader
}

// loadDeferred registers one lazy tool, executes it once and closes the registry:
// the single load recordLoadMetrics observes.
func loadDeferred(t *testing.T, name string) {
	t.Helper()
	lazy := tooltest.LazyTool(name, func(context.Context) (tool.Tool, error) {
		return tooltest.FuncTool(name, "", func(context.Context, string) (string, error) {
			return "ok", nil
		}), nil
	})
	registry, err := tool.NewRegistry([]tool.Source{tooltest.LazySource(lazy)})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Close() })

	proxy, ok := registry.Get(name)
	if !ok {
		t.Fatalf("lazy tool %q not registered", name)
	}
	if _, err := proxy.Execute(context.Background(), `{}`); err != nil {
		t.Fatalf("execute %q: %v", name, err)
	}
}

// metricNamed returns the metric called name, or fails the test when nothing was
// recorded under it: an absent series is a missing measurement, not an empty one.
func metricNamed(t *testing.T, collected metricdata.ResourceMetrics, name string) metricdata.Metrics {
	t.Helper()
	for _, scope := range collected.ScopeMetrics {
		for _, record := range scope.Metrics {
			if record.Name == name {
				return record
			}
		}
	}
	t.Fatalf("metric %q not recorded", name)
	return metricdata.Metrics{}
}
