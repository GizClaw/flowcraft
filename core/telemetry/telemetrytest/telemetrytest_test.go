package telemetrytest

import (
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
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
