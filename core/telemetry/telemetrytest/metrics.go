package telemetrytest

import (
	"context"
	"slices"
	"sort"
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
	out := Instruments{
		sums:       map[string][]Point{},
		histograms: map[string][]HistogramPoint{},
		shapes:     map[string]Definition{},
	}
	for _, scope := range collected.ScopeMetrics {
		for _, instrument := range scope.Metrics {
			out.shapes[instrument.Name] = shapeOf(instrument)
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
	shapes     map[string]Definition
}

// Definition is one instrument's declared shape: the part of a metric that no data point
// carries and every operator's query, alert threshold and dashboard axis depends on.
//
// It exists because a test that asserts the value of a series cannot see the difference
// between "duration in seconds" and "duration in milliseconds": both record a number, and
// only one of them is what the panel says.
type Definition struct {
	// Name is the instrument name as the meter declares it.
	Name string
	// Description is what the instrument counts, in the author's words.
	Description string
	// Unit is the declared unit ("s", "By", "{token}"), empty when none was declared.
	Unit string
	// Kind is the instrument kind, in the words the meter's constructors use.
	Kind string
	// Bounds are the histogram's bucket boundaries; empty for a non-histogram.
	Bounds []float64
}

// Definition returns the declared shape of one instrument, and whether it was part of this
// collection. An instrument that was created but never recorded is not in the collection:
// collection reports what was observed, not what exists.
func (i Instruments) Definition(name string) (Definition, bool) {
	shape, ok := i.shapes[name]
	return shape, ok
}

// Definitions lists every instrument in the collection, sorted by name, for a failure
// message that says what was there instead of only what was missing.
func (i Instruments) Definitions() []Definition {
	out := make([]Definition, 0, len(i.shapes))
	for _, shape := range i.shapes {
		out = append(out, shape)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

// Bounds returns a histogram's bucket boundaries.
//
// An instrument that is not a histogram carries none, and neither does one whose
// boundaries were left to the SDK default: both answers are "there is nothing to read
// here", which is what the second result says.
func (i Instruments) Bounds(name string) ([]float64, bool) {
	shape, ok := i.shapes[name]
	if !ok || len(shape.Bounds) == 0 {
		return nil, false
	}
	return shape.Bounds, true
}

// RequireShape asserts an instrument's declared kind and unit.
//
// The description is not pinned: it is prose that improves, and a test that fails on a
// better sentence is a test somebody deletes. What is pinned is what a query and an axis
// depend on — the kind (a counter and a gauge with the same name are two different
// questions) and the unit (a duration in seconds and one in milliseconds are the same
// number, one of them wrong).
//
// A histogram must also carry bucket boundaries: the SDK's default set is built for
// milliseconds, so a seconds-valued duration left on it reports every call above ten
// seconds as one bucket.
func (i Instruments) RequireShape(t *testing.T, name, kind, unit string) {
	t.Helper()
	shape, ok := i.shapes[name]
	if !ok {
		t.Fatalf("instrument %q was not emitted; this collection has %v", name, i.Names())
	}
	if shape.Kind != kind {
		t.Errorf("instrument %q is a %s, want a %s", name, shape.Kind, kind)
	}
	if shape.Unit != unit {
		t.Errorf("instrument %q declares unit %q, want %q", name, shape.Unit, unit)
	}
	if shape.Description == "" {
		t.Errorf("instrument %q has no description", name)
	}
	if kind == "histogram" && len(shape.Bounds) == 0 {
		t.Errorf("histogram %q has no bucket boundaries", name)
	}
}

// RequireBounds pins a histogram's bucket boundaries.
//
// It is separate from RequireShape because the boundaries are a decision about the data,
// not about the instrument: a test names them when the distribution is what it is watching.
func (i Instruments) RequireBounds(t *testing.T, name string, want []float64) {
	t.Helper()
	shape, ok := i.shapes[name]
	if !ok {
		t.Fatalf("instrument %q was not emitted; this collection has %v", name, i.Names())
	}
	if !slices.Equal(shape.Bounds, want) {
		t.Errorf("histogram %q has bounds %v, want %v", name, shape.Bounds, want)
	}
}

// shapeOf reads one collected metric's declaration.
func shapeOf(instrument metricdata.Metrics) Definition {
	shape := Definition{Name: instrument.Name, Description: instrument.Description, Unit: instrument.Unit}
	switch data := instrument.Data.(type) {
	case metricdata.Sum[int64]:
		shape.Kind = kindOf(data.IsMonotonic, true)
	case metricdata.Sum[float64]:
		shape.Kind = kindOf(data.IsMonotonic, true)
	case metricdata.Gauge[int64]:
		shape.Kind = "gauge"
	case metricdata.Gauge[float64]:
		shape.Kind = "gauge"
	case metricdata.Histogram[int64]:
		shape.Kind = "histogram"
		shape.Bounds = boundsOf(data.DataPoints)
	case metricdata.Histogram[float64]:
		shape.Kind = "histogram"
		shape.Bounds = boundsOf(data.DataPoints)
	}
	return shape
}

// kindOf names a sum the way the meter's constructors do: monotonic is the counter, and
// non-monotonic is the up-down counter.
func kindOf(monotonic, synchronous bool) string {
	if !synchronous {
		return "observable"
	}
	if monotonic {
		return "counter"
	}
	return "updowncounter"
}

// boundsOf is the first data point's bucket boundaries: they are a property of the
// instrument, and two points with different boundaries would be two instruments.
func boundsOf[T int64 | float64](points []metricdata.HistogramDataPoint[T]) []float64 {
	if len(points) == 0 {
		return nil
	}
	return points[0].Bounds
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
