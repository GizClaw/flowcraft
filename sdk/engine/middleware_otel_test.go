package engine_test

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// installTestTracer installs a TracerProvider that exports to an
// in-memory recorder, returning the recorder for assertions and a
// cleanup func that restores the previous global provider.
func installTestTracer(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	prev := otel.GetTracerProvider()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})
	return rec
}

func TestTracingMiddleware_PublishCreatesSpan(t *testing.T) {
	rec := installTestTracer(t)

	var publishedCalled bool
	base := engine.HostFuncs{
		Inner: engine.NoopHost{},
		PublishFn: func(_ context.Context, env event.Envelope) error {
			publishedCalled = true
			return nil
		},
	}
	host := engine.ComposeHost(base, engine.TracingMiddleware())

	subj := event.Subject("agent.run.started")
	env, err := event.NewEnvelope(context.Background(), subj, nil)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	env.Headers = map[string]string{event.HeaderRunID: "run-1"}

	if err := host.Publish(context.Background(), env); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !publishedCalled {
		t.Fatal("inner Publish must still be invoked")
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("want 1 ended span, got %d", len(spans))
	}
	if got := spans[0].Name(); got != "engine.host.publish" {
		t.Fatalf("span name = %q", got)
	}
	attrs := attrMap(spans[0].Attributes())
	if attrs["messaging.destination"] != string(subj) {
		t.Fatalf("missing destination attr: %#v", attrs)
	}
	if attrs["event.run_id"] != "run-1" {
		t.Fatalf("missing run_id attr: %#v", attrs)
	}
}

func TestTracingMiddleware_RecordsErrorOnFailure(t *testing.T) {
	rec := installTestTracer(t)

	wantErr := errors.New("publish boom")
	host := engine.ComposeHost(engine.HostFuncs{
		Inner: engine.NoopHost{},
		PublishFn: func(_ context.Context, _ event.Envelope) error {
			return wantErr
		},
	}, engine.TracingMiddleware())

	env, _ := event.NewEnvelope(context.Background(), "agent.run.started", nil)
	if err := host.Publish(context.Background(), env); !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}

	spans := rec.Ended()
	if len(spans) != 1 || spans[0].Status().Code.String() != "Error" {
		t.Fatalf("expected error-status span, got %+v", spans)
	}
	if len(spans[0].Events()) == 0 {
		t.Fatal("expected RecordError event on span")
	}
}

func TestTracingMiddleware_ReportUsageAttribs(t *testing.T) {
	rec := installTestTracer(t)
	host := engine.ComposeHost(engine.NoopHost{}, engine.TracingMiddleware())
	if err := host.ReportUsage(context.Background(), model.TokenUsage{
		Model: "gpt-4o", InputTokens: 12, OutputTokens: 34,
	}); err != nil {
		t.Fatalf("ReportUsage: %v", err)
	}
	spans := rec.Ended()
	if len(spans) != 1 || spans[0].Name() != "engine.host.report_usage" {
		t.Fatalf("spans = %+v", spans)
	}
	a := attrMap(spans[0].Attributes())
	if a["usage.model"] != "gpt-4o" || a["usage.input_tokens"] != int64(12) || a["usage.output_tokens"] != int64(34) {
		t.Fatalf("attrs = %#v", a)
	}
}

// attrMap flattens an OTel attribute KV slice into a Go map keyed by
// attribute key. Values are unwrapped to their native Go type so test
// assertions can use plain comparisons.
func attrMap(kvs []attribute.KeyValue) map[string]any {
	out := make(map[string]any, len(kvs))
	for _, kv := range kvs {
		out[string(kv.Key)] = kv.Value.AsInterface()
	}
	return out
}
