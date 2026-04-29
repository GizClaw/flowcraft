package engine_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/model"
)

func TestComposeHost_OrdersFirstSliceEntryAsOutermost(t *testing.T) {
	var seen []string

	mw := func(name string) engine.HostMiddleware {
		return func(inner engine.Host) engine.Host {
			return engine.HostFuncs{
				Inner: inner,
				ReportUsageFn: func(ctx context.Context, u model.TokenUsage) error {
					seen = append(seen, name)
					return inner.ReportUsage(ctx, u)
				},
			}
		}
	}

	composed := engine.ComposeHost(engine.NoopHost{}, mw("A"), mw("B"), mw("C"))
	if err := composed.ReportUsage(context.Background(), model.TokenUsage{}); err != nil {
		t.Fatalf("ReportUsage: %v", err)
	}
	want := []string{"A", "B", "C"}
	if len(seen) != 3 || seen[0] != want[0] || seen[1] != want[1] || seen[2] != want[2] {
		t.Fatalf("call order = %v, want %v (declaration order)", seen, want)
	}
}

func TestComposeHost_NoMiddlewaresEchoesBase(t *testing.T) {
	base := engine.NoopHost{}
	got := engine.ComposeHost(base)
	if _, ok := got.(engine.NoopHost); !ok {
		t.Fatalf("ComposeHost(base) with no middlewares must return base unchanged; got %T", got)
	}
}

func TestComposeHost_PanicsOnNilReturn(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on middleware returning nil Host")
		}
	}()
	_ = engine.ComposeHost(engine.NoopHost{}, func(engine.Host) engine.Host { return nil })
}

func TestHostFuncs_DelegatesUntouchedMethods(t *testing.T) {
	// Override only ReportUsage; every other method must fall through
	// to Inner. Verify by exercising each delegated method against
	// NoopHost and checking it gives NoopHost's documented behaviour.
	called := false
	wrapped := engine.HostFuncs{
		Inner: engine.NoopHost{},
		ReportUsageFn: func(_ context.Context, _ model.TokenUsage) error {
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
	if _, err := wrapped.AskUser(context.Background(), engine.UserPrompt{}); !errdefs.IsNotAvailable(err) {
		t.Errorf("AskUser should delegate to NoopHost (NotAvailable); got %v", err)
	}
	if err := wrapped.Checkpoint(context.Background(), engine.Checkpoint{}); err != nil {
		t.Errorf("Checkpoint should delegate to NoopHost (returns nil); got %v", err)
	}
	if err := wrapped.ReportUsage(context.Background(), model.TokenUsage{}); err != nil {
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

	gated := engine.HostFuncs{
		Inner: engine.NoopHost{},
		ReportUsageFn: func(_ context.Context, u model.TokenUsage) error {
			totalTokens += u.TotalTokens
			if totalTokens > quota {
				return errdefs.BudgetExceededf("token budget exceeded: %d/%d", totalTokens, quota)
			}
			return nil
		},
	}

	if err := gated.ReportUsage(context.Background(), model.TokenUsage{TotalTokens: 60}); err != nil {
		t.Fatalf("first call within budget; got %v", err)
	}
	err := gated.ReportUsage(context.Background(), model.TokenUsage{TotalTokens: 60})
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
	h := engine.HostFuncs{} // no overrides, no Inner
	_ = h.Publish(context.Background(), event.Envelope{Subject: "x"})
}
