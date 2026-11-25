package telemetrytest

import (
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

func TestRecorderCapturesSpansAndAttributes(t *testing.T) {
	recorder := New(t)

	ctx, parent := otel.Tracer("test").Start(t.Context(), "parent")
	_, child := otel.Tracer("test").Start(ctx, "tool.execute")
	child.SetAttributes(
		attribute.String("tool.name", "bash"),
		attribute.Int("tool.exit_code", 0),
	)
	child.AddEvent("policy.decision", trace.WithAttributes(
		attribute.String("policy.decision", "allow"),
	))
	child.SetStatus(codes.Ok, "OK")
	child.End()
	parent.End()

	got := recorder.RequireNamed(t, "tool.execute")
	RequireAttrString(t, got, "tool.name", "bash")
	RequireAttr(t, got, "tool.exit_code", attribute.IntValue(0))
	RequireEventCount(t, got, "policy.decision", 1)
	recorder.RequireCount(t, "parent", 1)
	recorder.RequireCount(t, "missing", 0)

	parentSpan := recorder.RequireNamed(t, "parent")
	if got.Parent().SpanID() != parentSpan.SpanContext().SpanID() {
		t.Fatalf("child parent = %s, want %s",
			got.Parent().SpanID(), parentSpan.SpanContext().SpanID())
	}

	recorder.Reset()
	if spans := recorder.Spans(); len(spans) != 0 {
		t.Fatalf("expected no spans after Reset, got %d", len(spans))
	}
}

// TestRecorderReadsInstrumentDefinitions: the kit is what every package's shape test
// builds on, so its own reading of a declaration is pinned here — including the two halves
// a value assertion cannot see, the unit and the bucket boundaries.
func TestRecorderReadsInstrumentDefinitions(t *testing.T) {
	recorder := Meters(t)
	meter := otel.Meter("test")

	counter, err := meter.Int64Counter("test.count", metric.WithDescription("Things counted"))
	if err != nil {
		t.Fatalf("Int64Counter: %v", err)
	}
	counter.Add(t.Context(), 1)

	histogram, err := meter.Float64Histogram("test.duration",
		metric.WithDescription("How long it took"),
		metric.WithUnit("s"),
		// 0, 1 and 2 seconds: small on purpose, so the assertion below is not a copy of
		// the SDK's default set.
		metric.WithExplicitBucketBoundaries(0, 1, 2))
	if err != nil {
		t.Fatalf("Float64Histogram: %v", err)
	}
	histogram.Record(t.Context(), 0.5)

	got := recorder.Collect(t)
	if count, ok := got.Sum("test.count"); !ok || count != 1 {
		t.Fatalf("test.count = %v (ok=%v), want 1", count, ok)
	}

	shape, ok := got.Definition("test.duration")
	if !ok {
		t.Fatalf("no definition for test.duration; collection has %v", got.Names())
	}
	if shape.Kind != "histogram" || shape.Unit != "s" || shape.Description != "How long it took" {
		t.Fatalf("test.duration = %+v, want a histogram in seconds", shape)
	}
	if bounds, _ := got.Bounds("test.duration"); len(bounds) != 3 {
		t.Fatalf("test.duration bounds = %v, want the three that were declared", bounds)
	}

	got.RequireShape(t, "test.duration", "histogram", "s")
	got.RequireShape(t, "test.count", "counter", "")
	got.RequireBounds(t, "test.duration", []float64{0, 1, 2})
}

// TestInstrumentsReportWhatTheyCarry covers the reading side of a collection: the
// definitions list, an instrument that was never emitted, and the two shapes a caller
// asks about rather than asserts on. These are the paths a package's shape test takes
// when it fails, so they are exercised here instead of only in the failing case.
func TestInstrumentsReportWhatTheyCarry(t *testing.T) {
	recorder := Meters(t)
	meter := otel.Meter("test")

	counter, err := meter.Int64Counter("test.calls", metric.WithDescription("Calls counted"))
	if err != nil {
		t.Fatalf("Int64Counter: %v", err)
	}
	counter.Add(t.Context(), 2)

	got := recorder.Collect(t)

	definitions := got.Definitions()
	if len(definitions) != 1 || definitions[0].Name != "test.calls" {
		t.Fatalf("Definitions() = %+v, want the one instrument", definitions)
	}
	if _, ok := got.Definition("test.missing"); ok {
		t.Fatal("Definition reported an instrument that was never emitted")
	}
	if _, ok := got.Bounds("test.calls"); ok {
		t.Fatal("Bounds reported bucket boundaries for a counter")
	}
	if value, ok := got.Sum("test.calls"); !ok || value != 2 {
		t.Fatalf("Sum = %v (ok=%v), want 2", value, ok)
	}
	if _, ok := got.SumWhere("test.calls", map[string]string{"kind": "input"}); ok {
		t.Fatal("SumWhere matched attributes the instrument does not carry")
	}
	if _, ok := got.HistogramCount("test.calls"); ok {
		t.Fatal("HistogramCount reported a counter")
	}
	if names := got.Names(); len(names) != 1 || names[0] != "test.calls" {
		t.Fatalf("Names() = %v", names)
	}
}
