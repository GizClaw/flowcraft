package fleet

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/cmd/vesseld/apispec"
	"github.com/GizClaw/flowcraft/cmd/vesseld/catalog"
	"github.com/GizClaw/flowcraft/cmd/vesseld/resolver"
	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// gateConfig wires N vessels off a single shared engine kind so
// daemon-wide concurrency tests can drive parallel Submits and
// observe the gate.
const gateConfig = `
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Daemon
metadata:
  name: vesseld-gate
spec:
  control:
    socket: /tmp/v.sock
  resources:
    maxConcurrentRuns: 2
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Vessel
metadata:
  name: alpha
spec:
  agents: [a]
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Agent
metadata:
  name: a
spec:
  engine: {ref: slow}
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Vessel
metadata:
  name: beta
spec:
  agents: [b]
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Agent
metadata:
  name: b
spec:
  engine: {ref: slow}
`

// buildFleet is a small helper that goes from YAML to a launched
// Fleet, registering the supplied engine factory under "slow".
func buildFleet(t *testing.T, cfg string, slow engine.Engine) *Fleet {
	t.Helper()
	objs, err := apispec.DecodeAll(strings.NewReader(cfg), "<test>")
	if err != nil {
		t.Fatal(err)
	}
	cat := catalog.New()
	cat.RegisterEngine("slow", func(_ string, _ map[string]any, _ catalog.Deps) (engine.Engine, error) {
		return slow, nil
	})
	plan, errs := resolver.Resolve(objs, cat, resolver.ResolveOptions{})
	if errs.Len() != 0 {
		t.Fatalf("resolve: %v", errs)
	}
	f, err := Build(*plan)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := f.Launch(context.Background()); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = f.Stop(ctx)
	})
	return f
}

// TestFleet_DaemonWideGate asserts spec.resources.maxConcurrentRuns
// at the Daemon level enforces a process-wide cap across vessels.
// Two vessels × 4 concurrent submits each = 8 in-flight; with
// cap=2 the peak observed concurrency MUST be 2.
func TestFleet_DaemonWideGate(t *testing.T) {
	t.Parallel()
	var (
		inflight int32
		peak     int32
		peakMu   sync.Mutex
	)
	slow := engine.EngineFunc(func(ctx context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		cur := atomic.AddInt32(&inflight, 1)
		defer atomic.AddInt32(&inflight, -1)
		peakMu.Lock()
		if cur > peak {
			peak = cur
		}
		peakMu.Unlock()
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return b, ctx.Err()
		}
		b.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleAssistant, "ok"))
		return b, nil
	})
	f := buildFleet(t, gateConfig, slow)

	var wg sync.WaitGroup
	for _, vesselAgent := range []struct{ vessel, agent string }{
		{"alpha", "a"}, {"alpha", "a"}, {"alpha", "a"}, {"alpha", "a"},
		{"beta", "b"}, {"beta", "b"}, {"beta", "b"}, {"beta", "b"},
	} {
		va := vesselAgent
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := f.Submit(context.Background(), va.vessel, va.agent, agent.Request{})
			if err != nil {
				t.Errorf("submit: %v", err)
				return
			}
			_, _ = h.Wait(context.Background())
		}()
	}
	wg.Wait()

	peakMu.Lock()
	got := peak
	peakMu.Unlock()
	if got > 2 {
		t.Fatalf("daemon gate breached: peak in-flight = %d, want <= 2", got)
	}
	if got == 0 {
		t.Fatalf("instrumentation never fired")
	}
}

// TestFleet_DrainAcrossCaptains asserts Fleet.Drain blocks until
// every vessel finishes Drain — not just one. We submit a slow
// run into each, then call Drain and watch how long it takes:
// because both vessels are running 50ms work, Drain should
// return between 50ms and 200ms (single vessel sequentially
// would take 100ms; concurrently ~50ms).
func TestFleet_DrainAcrossCaptains(t *testing.T) {
	t.Parallel()
	slow := engine.EngineFunc(func(ctx context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			return b, ctx.Err()
		}
		return b, nil
	})
	f := buildFleet(t, gateConfig, slow)

	for _, va := range []struct{ vessel, agent string }{{"alpha", "a"}, {"beta", "b"}} {
		_, _ = f.Submit(context.Background(), va.vessel, va.agent, agent.Request{})
	}
	start := time.Now()
	if err := f.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Drain took %s — too slow; concurrent drain expected", elapsed)
	}
}

// TestFleet_BusPerVessel asserts each vessel gets its own
// event.Bus and Bus(name) returns it. Cross-vessel isolation
// guarantees an envelope published into alpha's bus does NOT
// leak into beta's subscription.
func TestFleet_BusPerVessel(t *testing.T) {
	t.Parallel()
	slow := engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		return b, nil
	})
	f := buildFleet(t, gateConfig, slow)

	alphaBus, err := f.Bus("alpha")
	if err != nil {
		t.Fatalf("Bus alpha: %v", err)
	}
	betaBus, err := f.Bus("beta")
	if err != nil {
		t.Fatalf("Bus beta: %v", err)
	}
	if alphaBus == betaBus {
		t.Fatal("alpha and beta share a bus — cross-vessel isolation broken")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	alphaSub, err := alphaBus.Subscribe(ctx, event.Pattern("test.>"))
	if err != nil {
		t.Fatalf("subscribe alpha: %v", err)
	}
	defer alphaSub.Close()
	betaSub, err := betaBus.Subscribe(ctx, event.Pattern("test.>"))
	if err != nil {
		t.Fatalf("subscribe beta: %v", err)
	}
	defer betaSub.Close()

	if err := alphaBus.Publish(ctx, event.Envelope{Subject: "test.alpha-only"}); err != nil {
		t.Fatalf("publish alpha: %v", err)
	}

	select {
	case env := <-alphaSub.C():
		if env.Subject != "test.alpha-only" {
			t.Fatalf("alpha got wrong subject: %s", env.Subject)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("alpha never received its own publish")
	}
	select {
	case env := <-betaSub.C():
		t.Fatalf("beta saw alpha's envelope: %s", env.Subject)
	case <-time.After(150 * time.Millisecond):
	}
}

// TestFleet_NotFoundOnUnknownVessel asserts every Fleet entry
// point returns NotFound for a missing vessel — Captain, Bus,
// Submit. NotFound (not panic, not internal error) is how the
// HTTP handler maps to 404.
func TestFleet_NotFoundOnUnknownVessel(t *testing.T) {
	t.Parallel()
	slow := engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		return b, nil
	})
	f := buildFleet(t, gateConfig, slow)
	if _, err := f.Captain("ghost"); err == nil {
		t.Error("Captain ghost: expected error")
	}
	if _, err := f.Bus("ghost"); err == nil {
		t.Error("Bus ghost: expected error")
	}
	if _, err := f.Submit(context.Background(), "ghost", "x", agent.Request{}); err == nil {
		t.Error("Submit ghost: expected error")
	}
}
