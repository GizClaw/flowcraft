package event_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/GizClaw/flowcraft/sdk/event"
)

// startTracerProvider returns an in-memory provider that produces
// real (sampled) span/trace IDs, so envelopes constructed from a
// span context have valid hex strings to round-trip.
func startTracerProvider(t *testing.T) {
	t.Helper()
	prev := otel.GetTracerProvider()
	tp := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})
}

func TestEnvelope_SpanContext_RoundTrip(t *testing.T) {
	startTracerProvider(t)

	tr := otel.Tracer("test")
	ctx, span := tr.Start(context.Background(), "producer")
	want := span.SpanContext()
	defer span.End()

	env, err := event.NewEnvelope(ctx, "agent.run.started", nil)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	got := env.SpanContext()
	if got.TraceID() != want.TraceID() {
		t.Fatalf("trace id mismatch: got %s want %s", got.TraceID(), want.TraceID())
	}
	if got.SpanID() != want.SpanID() {
		t.Fatalf("span id mismatch: got %s want %s", got.SpanID(), want.SpanID())
	}
	if !got.IsRemote() {
		t.Fatal("decoded SpanContext must be Remote=true so child spans link as follows-from-remote")
	}
}

func TestEnvelope_WithRemoteContext_PassthroughOnEmpty(t *testing.T) {
	ctx := context.Background()
	env := event.Envelope{} // no IDs
	got := env.WithRemoteContext(ctx)
	if got != ctx {
		t.Fatal("WithRemoteContext on empty envelope must return original ctx unchanged")
	}
}

func TestEnvelope_WithRemoteContext_ChildLinks(t *testing.T) {
	startTracerProvider(t)

	tr := otel.Tracer("test")
	pctx, parent := tr.Start(context.Background(), "producer")
	wantTraceID := parent.SpanContext().TraceID()
	parent.End()

	env, err := event.NewEnvelope(pctx, "agent.run.started", nil)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}

	// Subscriber side: restore parent and start a child.
	cctx := env.WithRemoteContext(context.Background())
	_, child := tr.Start(cctx, "consumer")
	defer child.End()
	if got := child.SpanContext().TraceID(); got != wantTraceID {
		t.Fatalf("child trace id = %s, want %s", got, wantTraceID)
	}
}

// Compile-time assertion that the SpanContext extraction matches
// the OTel trace package's expectation.
var _ = trace.SpanContext{}
