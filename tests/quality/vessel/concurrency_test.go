package vesselquality

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/vessel"
	"github.com/GizClaw/flowcraft/vessel/spec"
)

// TestMaxConcurrentRunsCap asserts the Captain's admission gate
// honours spec.Resources.MaxConcurrentRuns: with cap=2 and a
// 200ms-per-run slow engine, six concurrent Submits MUST observe
// at most 2 in-flight executions at any moment.
//
// We use a hand-rolled engine.Engine here (not the fake LLM
// loop) because the assertion is on the gate, not on LLM
// behaviour — keeping the engine deterministic shrinks the
// surface area of what could go wrong.
func TestMaxConcurrentRunsCap(t *testing.T) {
	t.Parallel()
	const limit = 2
	const total = 6

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
		case <-time.After(150 * time.Millisecond):
		case <-ctx.Done():
			return b, ctx.Err()
		}
		b.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleAssistant, "ok"))
		return b, nil
	})

	vs := spec.Spec{
		ID:        "v-conc",
		Agents:    []spec.Agent{{Name: "primary"}},
		Resources: spec.Resources{MaxConcurrentRuns: limit, TurnTimeout: 5 * time.Second},
	}
	c := launchedCaptain(t, vs, vessel.WithEngine(slow))

	var wg sync.WaitGroup
	wg.Add(total)
	for i := 0; i < total; i++ {
		go func() {
			defer wg.Done()
			_, _ = c.Call(context.Background(), "primary", agent.Request{
				Message: model.NewTextMessage(model.RoleUser, "go"),
			})
		}()
	}
	wg.Wait()

	peakMu.Lock()
	finalPeak := peak
	peakMu.Unlock()
	if finalPeak == 0 {
		t.Fatalf("peak in-flight = 0 — instrumentation never fired")
	}
	if finalPeak > int32(limit) {
		t.Fatalf("peak in-flight runs %d exceeded cap %d", finalPeak, limit)
	}
}

// TestTurnTimeoutAborts asserts spec.Resources.TurnTimeout cancels
// a single run that exceeds the budget. We submit a 10-second
// engine sleep with a 80ms TurnTimeout — the run MUST return
// well under a second.
func TestTurnTimeoutAborts(t *testing.T) {
	t.Parallel()
	slow := engine.EngineFunc(func(ctx context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		select {
		case <-time.After(10 * time.Second):
			return b, nil
		case <-ctx.Done():
			return b, ctx.Err()
		}
	})

	vs := spec.Spec{
		ID:        "v-timeout",
		Agents:    []spec.Agent{{Name: "primary"}},
		Resources: spec.Resources{TurnTimeout: 80 * time.Millisecond},
	}
	c := launchedCaptain(t, vs, vessel.WithEngine(slow))

	start := time.Now()
	_, _ = c.Call(context.Background(), "primary", agent.Request{
		Message: model.NewTextMessage(model.RoleUser, "go"),
	})
	elapsed := time.Since(start)
	if elapsed > time.Second {
		t.Fatalf("turn timeout honoured too slowly: %s", elapsed)
	}
}
