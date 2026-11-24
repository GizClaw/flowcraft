package telemetrytest

import (
	"context"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// meterLock serializes the tests that install a meter provider, for the same reason
// providerLock does for spans: the provider is process-global, so two tests that collect
// at the same time would read each other's instruments.
var meterLock sync.Mutex

// Meters installs an in-memory meter provider for the duration of the test and returns a
// collector. Call it before the code under test resolves its instruments: a meter
// resolved before Meters keeps delegating to the previous provider.
//
// A metric is only observable after a collection, and collection is explicit on purpose:
// a test asserts on what it asked for, not on everything the process emitted while it was
// running. That is also why Collect returns the last value per instrument rather than a
// stream of updates.
func Meters(t *testing.T) *MetricRecorder {
	t.Helper()
	meterLock.Lock()
	previous := otel.GetMeterProvider()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetMeterProvider(previous)
		meterLock.Unlock()
	})
	return &MetricRecorder{reader: reader, ctx: context.Background()}
}

// MetricRecorder reads the instruments a test's code emitted.
type MetricRecorder struct {
	reader *sdkmetric.ManualReader
	ctx    context.Context
}

// Collect collects everything recorded since the provider was installed. Each call
// replaces the previous snapshot: a counter's value is cumulative, so a second Collect
// reports the total rather than the delta.
func (r *MetricRecorder) Collect(t *testing.T) Instruments {
	t.Helper()
	var collected metricdata.ResourceMetrics
	if err := r.reader.Collect(r.ctx, &collected); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	out := Instruments{sums: map[string][]Point{}, histograms: map[string][]HistogramPoint{}}
	for _, scope := range collected.ScopeMetrics {
		for _, instrument := range scope.Metrics {
			switch data := instrument.Data.(type) {
			case metricdata.Sum[int64]:
				for _, point := range data.DataPoints {
					out.sums[instrument.Name] = append(out.sums[instrument.Name],
						Point{Value: float64(point.Value), Attrs: attrsOf(point.Attributes)})
				}
			case metricdata.Sum[float64]:
				for _, point := range data.DataPoints {
					out.sums[instrument.Name] = append(out.sums[instrument.Name],
						Point{Value: point.Value, Attrs: attrsOf(point.Attributes)})
				}
			case metricdata.Histogram[float64]:
				for _, point := range data.DataPoints {
					out.histograms[instrument.Name] = append(out.histograms[instrument.Name],
						HistogramPoint{Count: point.Count, Attrs: attrsOf(point.Attributes)})
				}
			}
		}
	}
	return out
}

// Instruments is one collection, keyed by instrument name.
type Instruments struct {
	sums       map[string][]Point
	histograms map[string][]HistogramPoint
}

// Point is one sum data point.
type Point struct {
	Value float64
	Attrs map[string]string
}

// HistogramPoint is one histogram data point: the count is what a test can assert without
// depending on bucket boundaries.
type HistogramPoint struct {
	Count uint64
	Attrs map[string]string
}

// Sum returns the total of an instrument across all its attribute sets, and whether the
// instrument was emitted at all.
func (i Instruments) Sum(name string) (float64, bool) {
	points, ok := i.sums[name]
	if !ok {
		return 0, false
	}
	var total float64
	for _, point := range points {
		total += point.Value
	}
	return total, true
}

// SumWhere returns the total of the points whose attributes match want.
func (i Instruments) SumWhere(name string, want map[string]string) (float64, bool) {
	points, ok := i.sums[name]
	if !ok {
		return 0, false
	}
	var total float64
	matched := false
	for _, point := range points {
		if !matches(point.Attrs, want) {
			continue
		}
		total += point.Value
		matched = true
	}
	return total, matched
}

// HistogramCount returns how many observations an instrument recorded.
func (i Instruments) HistogramCount(name string) (uint64, bool) {
	points, ok := i.histograms[name]
	if !ok {
		return 0, false
	}
	var count uint64
	for _, point := range points {
		count += point.Count
	}
	return count, true
}

// Names lists the instruments that were emitted, for a failure message that says what was
// there instead of only what was missing.
func (i Instruments) Names() []string {
	names := make([]string, 0, len(i.sums)+len(i.histograms))
	for name := range i.sums {
		names = append(names, name)
	}
	for name := range i.histograms {
		names = append(names, name)
	}
	return names
}

func matches(have, want map[string]string) bool {
	for key, value := range want {
		if have[key] != value {
			return false
		}
	}
	return true
}

func attrsOf(set attribute.Set) map[string]string {
	out := map[string]string{}
	for iter := set.Iter(); iter.Next(); {
		pair := iter.Attribute()
		out[string(pair.Key)] = pair.Value.String()
	}
	return out
}
