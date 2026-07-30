package agent_test

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/inference"
)

func TestComposeHost_OrdersFirstSliceEntryAsOutermost(t *testing.T) {
	var seen []string

	mw := func(name string) agent.HostMiddleware {
		return func(inner agent.Host) agent.Host {
			return agent.HostFuncs{
				Inner: inner,
				ReportUsageFn: func(ctx context.Context, u inference.Usage) error {
					seen = append(seen, name)
					return inner.ReportUsage(ctx, u)
				},
			}
		}
	}

	composed := agent.ComposeHost(agent.NoopHost{}, mw("A"), mw("B"), mw("C"))
	if err := composed.ReportUsage(context.Background(), inference.Usage{}); err != nil {
		t.Fatalf("ReportUsage: %v", err)
	}
	want := []string{"A", "B", "C"}
	if len(seen) != 3 || seen[0] != want[0] || seen[1] != want[1] || seen[2] != want[2] {
		t.Fatalf("call order = %v, want %v (declaration order)", seen, want)
	}
}

func TestComposeHost_NoMiddlewaresEchoesBase(t *testing.T) {
	base := agent.NoopHost{}
	got := agent.ComposeHost(base)
	if _, ok := got.(agent.NoopHost); !ok {
		t.Fatalf("ComposeHost(base) with no middlewares must return base unchanged; got %T", got)
	}
}

func TestComposeHost_PanicsOnNilReturn(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on middleware returning nil Host")
		}
	}()
	_ = agent.ComposeHost(agent.NoopHost{}, func(agent.Host) agent.Host { return nil })
}

func TestHostFuncs_DelegatesUntouchedMethods(t *testing.T) {
	// Override only ReportUsage; every other method must fall through
	// to Inner. Verify by exercising each delegated method against
	// NoopHost and checking it gives NoopHost's documented behaviour.
	called := false
	wrapped := agent.HostFuncs{
		Inner: agent.NoopHost{},
		ReportUsageFn: func(_ context.Context, _ inference.Usage) error {
			called = true
			return nil
		},
	}

	if err := wrapped.Publish(context.Background(), event.Envelope{Subject: "x"}); err != nil {
		t.Errorf("Publish should delegate to NoopHost (returns nil); got %v", err)
	}
	if wrapped.Interrupts() != nil {
		t.Error("Interrupts should delegate to NoopHost (returns nil)")
	}
	if _, err := wrapped.AskUser(context.Background(), agent.UserPrompt{}); !errdefs.IsNotAvailable(err) {
		t.Errorf("AskUser should delegate to NoopHost (NotAvailable); got %v", err)
	}
	if err := wrapped.Checkpoint(context.Background(), agent.Checkpoint{}); err != nil {
		t.Errorf("Checkpoint should delegate to NoopHost (returns nil); got %v", err)
	}
	if err := wrapped.ReportUsage(context.Background(), inference.Usage{}); err != nil {
		t.Errorf("ReportUsage override returned %v, want nil", err)
	}
	if !called {
		t.Error("ReportUsageFn override was never invoked")
	}
}

func TestHostFuncs_BudgetGateRefusesNextCall(t *testing.T) {
	// Worked example: the canonical sandbox host wraps base so
	// ReportUsage returns BudgetExceeded once a quota hits. Engines
	// observing the error must propagate; we assert the wire-level
	// classification here so a refactor that loses the marker breaks
	// loudly.
	var totalTokens int64
	const quota = int64(100)

	gated := agent.HostFuncs{
		Inner: agent.NoopHost{},
		ReportUsageFn: func(_ context.Context, u inference.Usage) error {
			totalTokens += u.TotalTokens
			if totalTokens > quota {
				return errdefs.BudgetExceededf("token budget exceeded: %d/%d", totalTokens, quota)
			}
			return nil
		},
	}

	if err := gated.ReportUsage(context.Background(), inference.Usage{TotalTokens: 60}); err != nil {
		t.Fatalf("first call within budget; got %v", err)
	}
	err := gated.ReportUsage(context.Background(), inference.Usage{TotalTokens: 60})
	if !errdefs.IsBudgetExceeded(err) {
		t.Fatalf("second call must trip BudgetExceeded; got %v", err)
	}
}

func TestHostFuncs_NilInnerPanicsClearly(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic when Inner is nil and method is delegated")
		}
	}()
	h := agent.HostFuncs{} // no overrides, no Inner
	_ = h.Publish(context.Background(), event.Envelope{Subject: "x"})
}

// ---------- OTel tracing middleware ----------

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
	base := agent.HostFuncs{
		Inner: agent.NoopHost{},
		PublishFn: func(_ context.Context, env event.Envelope) error {
			publishedCalled = true
			return nil
		},
	}
	host := agent.ComposeHost(base, agent.TracingMiddleware())

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
	if got := spans[0].Name(); got != "agent.host.publish" {
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
	host := agent.ComposeHost(agent.HostFuncs{
		Inner: agent.NoopHost{},
		PublishFn: func(_ context.Context, _ event.Envelope) error {
			return wantErr
		},
	}, agent.TracingMiddleware())

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
	host := agent.ComposeHost(agent.NoopHost{}, agent.TracingMiddleware())
	if err := host.ReportUsage(context.Background(), inference.Usage{
		Model: inference.ModelRef{
			ID: inference.ModelID{Provider: "openai", Name: "gpt-4o"},
		},
		InputTokens: 12, OutputTokens: 34,
	}); err != nil {
		t.Fatalf("ReportUsage: %v", err)
	}
	spans := rec.Ended()
	if len(spans) != 1 || spans[0].Name() != "agent.host.report_usage" {
		t.Fatalf("spans = %+v", spans)
	}
	a := attrMap(spans[0].Attributes())
	if a["usage.provider"] != "openai" || a["usage.model"] != "gpt-4o" ||
		a["usage.input_tokens"] != int64(12) || a["usage.output_tokens"] != int64(34) {
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
