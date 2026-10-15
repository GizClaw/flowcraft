package event

import (
	"context"
	"os"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// busMetricsReader receives every metric this package's tests record. It
// is installed once for the whole test binary in TestMain because the
// bus instruments are created at package init, before any provider
// exists: the OpenTelemetry global provider resolves them when the first
// provider is installed, and never re-resolves them afterwards, so a
// reader installed by an individual test would only ever see the
// instruments' output if it happened to be that first provider.
var busMetricsReader = sdkmetric.NewManualReader()

func TestMain(m *testing.M) {
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(busMetricsReader)))
	code := m.Run()
	otel.SetMeterProvider(previous)
	os.Exit(code)
}

// collectedDrops reads the bus drop counter, keyed by drop reason. The
// tally is process-wide (the bus instruments are package-level), so
// tests compare against a baseline rather than against absolute values.
func collectedDrops(t *testing.T) map[string]int64 {
	t.Helper()
	var collected metricdata.ResourceMetrics
	if err := busMetricsReader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	out := map[string]int64{}
	for _, scope := range collected.ScopeMetrics {
		for _, record := range scope.Metrics {
			if record.Name != "drops.total" {
				continue
			}
			sum, ok := record.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("metric drops.total has type %T, want an int64 sum", record.Data)
			}
			for _, point := range sum.DataPoints {
				reason, _ := point.Attributes.Value(attribute.Key("event.drop_reason"))
				out[reason.AsString()] += point.Value
			}
		}
	}
	return out
}

// TestMemoryBusReportsDropsWithoutObserver is the regression test for
// silent loss: with no observer attached (the default — the bus keeps
// noopObserver and the guarded branch meant to log drops could never
// run), a dropped envelope must still reach the bus's own metrics.
func TestMemoryBusReportsDropsWithoutObserver(t *testing.T) {
	before := collectedDrops(t)

	bus := NewMemoryBus()
	defer func() { _ = bus.Close() }()
	ctx := context.Background()

	// A buffer one deep: the second publish has nowhere to go and the
	// default DropNewest policy drops it.
	sub, err := bus.Subscribe(ctx, Pattern("drop.test.>"), WithBufferSize(1))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = sub.Close() }()
	for i := range 2 {
		if err := bus.Publish(ctx, mustEnv(t, "drop.test.subject", map[string]int{"i": i})); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}
	if got := bus.Dropped(); got != 1 {
		t.Fatalf("Dropped() = %d, want 1", got)
	}

	after := collectedDrops(t)
	if got := after[DropReasonBufferFull.String()] - before[DropReasonBufferFull.String()]; got != 1 {
		t.Fatalf("buffer_full drops recorded = %d, want 1 (before %v, after %v)",
			got, before, after)
	}
}

// TestMemoryBusLabelsVoluntarySampling proves the counter separates a
// Sample-policy rejection from real loss, which is what lets the log
// stay quiet for a sampling stream while still reporting loss.
func TestMemoryBusLabelsVoluntarySampling(t *testing.T) {
	before := collectedDrops(t)

	bus := NewMemoryBus()
	defer func() { _ = bus.Close() }()
	ctx := context.Background()

	// Rate 0 rejects every envelope, so the publish is dropped by the
	// sampling gate rather than by a full buffer.
	sub, err := bus.Subscribe(ctx, Pattern("sample.test.>"),
		WithBackpressure(Sample), WithSampleRate(0))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = sub.Close() }()
	if err := bus.Publish(ctx, mustEnv(t, "sample.test.subject", nil)); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	after := collectedDrops(t)
	if got := after[DropReasonSampled.String()] - before[DropReasonSampled.String()]; got != 1 {
		t.Fatalf("sampled drops recorded = %d, want 1 (before %v, after %v)",
			got, before, after)
	}
	if got := after[DropReasonBufferFull.String()] - before[DropReasonBufferFull.String()]; got != 0 {
		t.Fatalf("buffer_full drops recorded = %d, want 0", got)
	}
}

// TestClaimDropLogRateLimits pins the log throttle: one summary per bus
// per interval, however many publishers are dropping.
func TestClaimDropLogRateLimits(t *testing.T) {
	bus := NewMemoryBus()
	now := time.Now()

	if !bus.claimDropLog(now) {
		t.Fatal("first drop was not logged")
	}
	if bus.claimDropLog(now.Add(time.Second)) {
		t.Fatal("second drop within the interval was logged")
	}
	if !bus.claimDropLog(now.Add(dropLogInterval + time.Millisecond)) {
		t.Fatal("drop after the interval was not logged")
	}
}
