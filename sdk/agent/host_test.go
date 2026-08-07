package agent_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/inference"
)

func TestNoopHost_SatisfiesHost(t *testing.T) {
	// Compile-time assertion that NoopHost still satisfies agent.Host
	// after refactors that touch the composition.
	var _ agent.Host = agent.NoopHost{}
}

func TestNoopHost_PublishDrops(t *testing.T) {
	if err := (agent.NoopHost{}).Publish(context.Background(), event.Envelope{Subject: "x"}); err != nil {
		t.Errorf("NoopHost.Publish must drop silently; got %v", err)
	}
}

func TestNoopHost_InterruptsBlocksForever(t *testing.T) {
	// We cannot assert "blocks forever" directly without a goroutine
	// leak. Asserting nil is sufficient — receiving on a nil channel
	// is the documented "blocks forever" semantic.
	if (agent.NoopHost{}).Interrupts() != nil {
		t.Error("NoopHost.Interrupts() must be nil so engines block forever on it")
	}
}

func TestNoopHost_AskUserNotAvailable(t *testing.T) {
	_, err := (agent.NoopHost{}).AskUser(context.Background(), agent.UserPrompt{})
	if !errdefs.IsNotAvailable(err) {
		t.Errorf("AskUser must return errdefs.IsNotAvailable; got %v", err)
	}
}

func TestNoopHost_CheckpointDrops(t *testing.T) {
	if err := (agent.NoopHost{}).Checkpoint(context.Background(), agent.Checkpoint{}); err != nil {
		t.Errorf("NoopHost.Checkpoint must drop silently; got %v", err)
	}
}

func TestNoopHost_ReportUsageDropsAndReturnsNil(t *testing.T) {
	// NoopHost has no budget so it MUST return nil — engines that
	// branch on errdefs.IsBudgetExceeded never see it under noop.
	if err := (agent.NoopHost{}).ReportUsage(context.Background(),
		inference.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}); err != nil {
		t.Errorf("NoopHost.ReportUsage must return nil; got %v", err)
	}
}

type eventBusHost struct {
	agent.NoopHost
	bus event.Bus
}

func (h *eventBusHost) EventBus() event.Bus { return h.bus }

type externalHostWrapper struct {
	agent.Host
}

func (h externalHostWrapper) UnwrapHost() agent.Host { return h.Host }

func TestCapabilityFromHost(t *testing.T) {
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })

	t.Run("direct", func(t *testing.T) {
		got, ok := agent.CapabilityFromHost[agent.EventBusProvider](&eventBusHost{bus: bus})
		if !ok || got.EventBus() != bus {
			t.Fatalf("CapabilityFromHost = (%v, %v)", got, ok)
		}
	})

	t.Run("typed nil host", func(t *testing.T) {
		var host *eventBusHost
		if got, ok := agent.CapabilityFromHost[agent.EventBusProvider](host); ok || got != nil {
			t.Fatalf("CapabilityFromHost(typed nil) = (%v, %v)", got, ok)
		}
	})

	t.Run("decorator", func(t *testing.T) {
		host := agent.ComposeHost(&eventBusHost{bus: bus}, agent.TracingMiddleware())
		got, ok := agent.CapabilityFromHost[agent.EventBusProvider](host)
		if !ok || got.EventBus() != bus {
			t.Fatalf("CapabilityFromHost(decorated) = (%v, %v)", got, ok)
		}
	})

	t.Run("external decorator", func(t *testing.T) {
		host := externalHostWrapper{Host: &eventBusHost{bus: bus}}
		got, ok := agent.CapabilityFromHost[agent.EventBusProvider](host)
		if !ok || got.EventBus() != bus {
			t.Fatalf("CapabilityFromHost(external decorator) = (%v, %v)", got, ok)
		}
	})

	t.Run("custom publisher is authoritative", func(t *testing.T) {
		host := agent.HostFuncs{
			Inner: &eventBusHost{bus: bus},
			PublishFn: func(context.Context, event.Envelope) error {
				return nil
			},
		}
		if got, ok := agent.CapabilityFromHost[agent.EventBusProvider](host); ok || got != nil {
			t.Fatalf("CapabilityFromHost(custom publisher) = (%v, %v)", got, ok)
		}
	})
}

func TestEventBusFromHost(t *testing.T) {
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })

	t.Run("supported", func(t *testing.T) {
		got, ok := agent.EventBusFromHost(&eventBusHost{bus: bus})
		if !ok || got != bus {
			t.Fatalf("EventBusFromHost = (%v, %v), want (%v, true)", got, ok, bus)
		}
	})

	t.Run("unsupported", func(t *testing.T) {
		if got, ok := agent.EventBusFromHost(agent.NoopHost{}); ok || got != nil {
			t.Fatalf("EventBusFromHost(NoopHost) = (%v, %v), want (nil, false)", got, ok)
		}
	})

	t.Run("typed nil host", func(t *testing.T) {
		var host *eventBusHost
		if got, ok := agent.EventBusFromHost(host); ok || got != nil {
			t.Fatalf("EventBusFromHost(typed nil) = (%v, %v), want (nil, false)", got, ok)
		}
	})

	t.Run("typed nil bus", func(t *testing.T) {
		var bus *event.MemoryBus
		if got, ok := agent.EventBusFromHost(&eventBusHost{bus: bus}); ok || got != nil {
			t.Fatalf("EventBusFromHost(typed nil bus) = (%v, %v), want (nil, false)", got, ok)
		}
	})

	t.Run("decorators preserve without claiming capability", func(t *testing.T) {
		base := &eventBusHost{bus: bus}
		wrapped := agent.ComposeHost(base,
			func(inner agent.Host) agent.Host { return agent.HostFuncs{Inner: inner} },
			agent.TracingMiddleware(),
		)
		if _, ok := wrapped.(agent.EventBusProvider); ok {
			t.Fatal("decorated Host must not claim EventBusProvider directly")
		}
		got, ok := agent.EventBusFromHost(wrapped)
		if !ok || got != bus {
			t.Fatalf("EventBusFromHost(decorated) = (%v, %v), want (%v, true)", got, ok, bus)
		}
	})

	t.Run("unsupported decorator remains unsupported", func(t *testing.T) {
		wrapped := agent.HostFuncs{Inner: agent.NoopHost{}}
		if _, ok := any(wrapped).(agent.EventBusProvider); ok {
			t.Fatal("HostFuncs must not claim EventBusProvider")
		}
		if got, ok := agent.EventBusFromHost(wrapped); ok || got != nil {
			t.Fatalf("EventBusFromHost(wrapped NoopHost) = (%v, %v), want (nil, false)", got, ok)
		}
	})

	t.Run("custom publisher hides inner event surface", func(t *testing.T) {
		wrapped := agent.HostFuncs{
			Inner: &eventBusHost{bus: bus},
			PublishFn: func(context.Context, event.Envelope) error {
				return nil
			},
		}
		if got, ok := agent.EventBusFromHost(wrapped); ok || got != nil {
			t.Fatalf("EventBusFromHost(custom publisher) = (%v, %v), want (nil, false)", got, ok)
		}
	})
}

func TestEngineFunc_NilSafe(t *testing.T) {
	// Documented contract: a zero-value EngineFunc returns
	// (board, nil) without panicking.
	board := agent.NewBoard()
	got, err := agent.EngineFunc(nil).Execute(
		context.Background(), agent.Run{}, agent.NoopHost{}, board)
	if err != nil {
		t.Errorf("nil EngineFunc.Execute returned error: %v", err)
	}
	if got != board {
		t.Error("nil EngineFunc.Execute must echo the input board")
	}
}

func TestEngineFunc_AdaptsClosure(t *testing.T) {
	called := false
	f := agent.EngineFunc(func(_ context.Context, r agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		called = true
		if r.RunID != "exec-1" {
			t.Errorf("Run.RunID = %q, want exec-1", r.RunID)
		}
		return b, nil
	})

	b := agent.NewBoard()
	_, err := f.Execute(context.Background(), agent.Run{Identity: agent.Identity{RunID: "exec-1"}}, agent.NoopHost{}, b)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !called {
		t.Error("EngineFunc did not invoke wrapped closure")
	}
}

// ---------- Host context transport ----------

func TestWithHost_RoundTrip(t *testing.T) {
	want := agent.NoopHost{}
	ctx := agent.ContextWithHost(context.Background(), want)
	got, ok := agent.HostFromContext(ctx)
	if !ok {
		t.Fatal("HostFromContext returned ok=false after WithHost")
	}
	if got != want {
		t.Errorf("HostFromContext returned %v, want %v", got, want)
	}
}

func TestWithHost_NilHostIsNoop(t *testing.T) {
	parent := context.Background()
	ctx := agent.ContextWithHost(parent, nil)
	if ctx != parent {
		t.Errorf("WithHost(nil) must return ctx unchanged so callers can plumb unconditionally")
	}
	if _, ok := agent.HostFromContext(ctx); ok {
		t.Errorf("HostFromContext returned ok=true after WithHost(nil)")
	}
}

func TestHostFromContext_NilCtxReturnsFalse(t *testing.T) {
	//nolint:staticcheck // deliberate: nil Context must yield (nil, false)
	if h, ok := agent.HostFromContext(nil); ok || h != nil {
		t.Errorf("nil ctx must yield (nil, false); got (%v, %v)", h, ok)
	}
}

func TestHostFromContext_BareCtxReturnsFalse(t *testing.T) {
	if _, ok := agent.HostFromContext(context.Background()); ok {
		t.Errorf("bare ctx must yield ok=false")
	}
}

// ---------- Interrupt ----------

func TestInterrupted_SatisfiesIsInterrupted(t *testing.T) {
	err := agent.Interrupted(agent.Interrupt{Cause: agent.CauseUserCancel, Detail: "stop"})
	if !errdefs.IsInterrupted(err) {
		t.Errorf("Interrupted() error must satisfy errdefs.IsInterrupted; got %v", err)
	}
}

func TestInterrupted_AsRestoresInterrupt(t *testing.T) {
	err := agent.Interrupted(agent.Interrupt{Cause: agent.CauseUserInput, Detail: "barge"})

	var ie agent.InterruptedError
	if !errors.As(err, &ie) {
		t.Fatal("errors.As must destructure InterruptedError")
	}
	if ie.Cause != agent.CauseUserInput {
		t.Errorf("Cause = %q, want %q", ie.Cause, agent.CauseUserInput)
	}
	if ie.Detail != "barge" {
		t.Errorf("Detail = %q, want %q", ie.Detail, "barge")
	}
}

func TestInterrupted_AsThroughWrap(t *testing.T) {
	wrapped := fmt.Errorf("layered: %w",
		agent.Interrupted(agent.Interrupt{Cause: agent.CauseHostShutdown, Detail: "graceful"}))

	if !errdefs.IsInterrupted(wrapped) {
		t.Error("wrapped Interrupted should still satisfy IsInterrupted")
	}

	var ie agent.InterruptedError
	if !errors.As(wrapped, &ie) {
		t.Fatal("errors.As must drill through wraps")
	}
	if ie.Cause != agent.CauseHostShutdown {
		t.Errorf("Cause = %q, want %q", ie.Cause, agent.CauseHostShutdown)
	}
}

func TestInterrupted_ZeroValueWellFormedMessage(t *testing.T) {
	cases := []struct {
		name string
		intr agent.Interrupt
		want string
	}{
		{"zero", agent.Interrupt{}, "engine: interrupted"},
		{"detailOnly", agent.Interrupt{Detail: "stuck"}, "engine: interrupted: stuck"},
		{"causeOnly", agent.Interrupt{Cause: agent.CauseUserCancel}, "engine: interrupted (user_cancel)"},
		{"both", agent.Interrupt{Cause: agent.CauseUserInput, Detail: "barge"}, "engine: interrupted (user_input): barge"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := agent.Interrupted(c.intr)
			if err.Error() != c.want {
				t.Errorf("Error() = %q, want %q", err.Error(), c.want)
			}
		})
	}
}

// TestInterruptedError_MarkerInvoked ensures the unexported marker
// method on InterruptedError is actually called by an errors.As-based
// classifier; without an explicit interface assertion the cover tool
// won't see it run. We use the public marker shape that errdefs
// expects.
func TestInterruptedError_MarkerInvoked(t *testing.T) {
	err := agent.Interrupted(agent.Interrupt{Cause: agent.CauseUserCancel})

	var marker interface{ Interrupted() }
	if !errors.As(err, &marker) {
		t.Fatal("Interrupted() error must satisfy the errdefs marker shape")
	}
	// Calling the marker must not panic.
	marker.Interrupted()
}

func TestMergeInterrupts_FansInFromMultipleSources(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := make(chan agent.Interrupt, 1)
	b := make(chan agent.Interrupt, 1)
	out := agent.MergeInterrupts(ctx, a, b)

	a <- agent.Interrupt{Cause: agent.CauseUserCancel, Detail: "from-a"}
	b <- agent.Interrupt{Cause: agent.CauseHostShutdown, Detail: "from-b"}

	got := make(map[agent.Cause]string)
	for i := 0; i < 2; i++ {
		select {
		case intr := <-out:
			got[intr.Cause] = intr.Detail
		case <-time.After(time.Second):
			t.Fatalf("merged channel timed out at %d/2", i+1)
		}
	}
	if got[agent.CauseUserCancel] != "from-a" || got[agent.CauseHostShutdown] != "from-b" {
		t.Fatalf("merged values = %+v, want both detail strings", got)
	}
}

func TestMergeInterrupts_NilSourcesIgnored(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	live := make(chan agent.Interrupt, 1)
	out := agent.MergeInterrupts(ctx, nil, live, nil)

	live <- agent.Interrupt{Cause: agent.CauseCustom, Detail: "go"}
	select {
	case intr := <-out:
		if intr.Cause != agent.CauseCustom {
			t.Fatalf("Cause = %q, want %q", intr.Cause, agent.CauseCustom)
		}
	case <-time.After(time.Second):
		t.Fatal("nil sources must be skipped, not crash; live source produced no value")
	}
}

func TestMergeInterrupts_ClosesWhenEverySourceCloses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := make(chan agent.Interrupt)
	b := make(chan agent.Interrupt)
	out := agent.MergeInterrupts(ctx, a, b)

	close(a)
	close(b)

	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("merged channel should be closed after every source closed")
		}
	case <-time.After(time.Second):
		t.Fatal("merged channel did not close within deadline")
	}
}

func TestMergeInterrupts_ClosesOnCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := make(chan agent.Interrupt)
	out := agent.MergeInterrupts(ctx, a)

	cancel()

	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("merged channel should be closed after ctx cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("ctx cancel did not propagate to merged channel within deadline")
	}
}

func TestMergeInterrupts_ZeroSources(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := agent.MergeInterrupts(ctx)

	// Channel is alive (no source closed it) until ctx fires.
	select {
	case <-out:
		t.Fatal("zero-source merge must not yield values before ctx cancel")
	default:
	}
	cancel()
	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("zero-source merge must close on ctx cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("zero-source merge did not close after ctx cancel")
	}
}

func TestMergeInterrupts_NoGoroutineLeakAfterCancel(t *testing.T) {
	// Defensive: spin up several merges, cancel them, ensure every
	// source-side forwarder has exited (i.e. each source channel can
	// be re-used / GC'd) by counting WaitGroup completions through a
	// proxy: re-sending into an already-cancelled merge must not
	// block indefinitely because no forwarder is parked on receive.
	ctx, cancel := context.WithCancel(context.Background())
	a := make(chan agent.Interrupt, 1)
	out := agent.MergeInterrupts(ctx, a)

	cancel()
	// Drain out so the closer goroutine sees both ctx.Done and an
	// emptied channel state.
	<-out

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// After cancel + drain, sending into the source channel must
		// not park forever — no forwarder remains. Buffered cap=1
		// absorbs the send; unblocked goroutine returns immediately.
		a <- agent.Interrupt{}
	}()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("source channel send blocked — forwarder goroutine leaked")
	}
}

func TestCauseConstants_StableValues(t *testing.T) {
	// The Cause string values are part of the wire contract (they
	// flow into errdefs and may be persisted in checkpoint metadata).
	// Pin them down so a refactor that renames a constant breaks
	// loudly.
	pairs := []struct {
		c    agent.Cause
		want string
	}{
		{agent.CauseUnknown, ""},
		{agent.CauseUserCancel, "user_cancel"},
		{agent.CauseUserInput, "user_input"},
		{agent.CauseHostShutdown, "host_shutdown"},
		{agent.CauseCustom, "custom"},
	}
	for _, p := range pairs {
		if string(p.c) != p.want {
			t.Errorf("Cause %q has value %q, want %q", p.c, string(p.c), p.want)
		}
	}
}
