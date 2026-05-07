package vessel

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/vessel/spec"
)

// TestSandbox_MaxConcurrentRuns enforces the semaphore: with cap=1
// and a slow engine, the second Submit must block until the first
// finishes. We assert via timing that the second response arrives
// strictly after the first gate is released.
func TestSandbox_MaxConcurrentRuns(t *testing.T) {
	t.Parallel()
	gate := make(chan struct{})
	var entered atomic.Int32
	slow := engine.EngineFunc(func(ctx context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		entered.Add(1)
		select {
		case <-gate:
		case <-ctx.Done():
			return b, ctx.Err()
		}
		b.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleAssistant, "ok"))
		return b, nil
	})

	vs := spec.Spec{
		Agents:    []spec.Agent{{Name: "p"}},
		Resources: spec.Resources{MaxConcurrentRuns: 1},
	}
	c, err := New(vs, WithEngine(slow))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Stop(context.Background())
	_ = c.Launch(context.Background())

	h1, _ := c.Submit(context.Background(), "p", agent.Request{Message: model.NewTextMessage(model.RoleUser, "1")})
	h2, _ := c.Submit(context.Background(), "p", agent.Request{Message: model.NewTextMessage(model.RoleUser, "2")})

	// Wait briefly to confirm only one engine entered. Submit is
	// async — Submit's goroutine acquires the slot, so race the
	// short sleep against the semaphore wait.
	time.Sleep(40 * time.Millisecond)
	if got := entered.Load(); got != 1 {
		t.Fatalf("expected 1 concurrent engine; got %d", got)
	}

	// Release both runs (one fires now, the other after the first
	// releases its slot). Wait for both handles in parallel so the
	// test does not assume Submit-order = execution-order.
	go func() { gate <- struct{}{}; gate <- struct{}{} }()

	type ret struct {
		res *agent.Result
		err error
	}
	results := make(chan ret, 2)
	for _, h := range []*Handle{h1, h2} {
		go func(h *Handle) {
			r, e := h.Wait(context.Background())
			results <- ret{r, e}
		}(h)
	}
	for i := 0; i < 2; i++ {
		select {
		case r := <-results:
			if r.err != nil {
				t.Fatalf("Wait: %v", r.err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("Wait timed out (i=%d, entered=%d)", i, entered.Load())
		}
	}
	if got := entered.Load(); got != 2 {
		t.Fatalf("expected 2 engines to have run, got %d", got)
	}
}

// TestSandbox_TurnTimeout enforces the per-Run deadline: the engine
// blocks past the deadline and observes ctx.Done().
func TestSandbox_TurnTimeout(t *testing.T) {
	t.Parallel()
	hung := engine.EngineFunc(func(ctx context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		<-ctx.Done()
		return b, ctx.Err()
	})
	vs := spec.Spec{
		Agents:    []spec.Agent{{Name: "p"}},
		Resources: spec.Resources{TurnTimeout: 50 * time.Millisecond},
	}
	c, err := New(vs, WithEngine(hung))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Stop(context.Background())
	_ = c.Launch(context.Background())

	res, err := c.Call(context.Background(), "p", agent.Request{Message: model.NewTextMessage(model.RoleUser, "x")})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res == nil || res.Status != agent.StatusCanceled {
		t.Fatalf("res = %v, want StatusCanceled", res)
	}
	if !errors.Is(res.Err, context.DeadlineExceeded) {
		t.Fatalf("res.Err = %v, want DeadlineExceeded", res.Err)
	}
}

// TestSandbox_ToolAllowlistPropagated verifies the vs.Tools list
// reaches agent.Agent.Tools — the engine's policy gate. We do not
// drive a real registry here; the assertion is on the value the
// Captain's EngineFactory observes.
func TestSandbox_ToolAllowlistPropagated(t *testing.T) {
	t.Parallel()
	var seen []string
	vs := spec.Spec{
		Agents: []spec.Agent{{Name: "p", Tools: []string{"calc", "search"}}},
	}
	c, err := New(vs, WithEngineFactory(func(a spec.Agent, _ Deps) (engine.Engine, error) {
		seen = append([]string(nil), a.Tools...)
		return echoEngine(), nil
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Stop(context.Background())

	if len(seen) != 2 || seen[0] != "calc" || seen[1] != "search" {
		t.Fatalf("factory saw tools=%v, want [calc search]", seen)
	}
	// The buildAgentValue copy also propagates onto agent.Agent.
	entry := c.entries["p"]
	if got := entry.agent.Tools; len(got) != 2 || got[0] != "calc" {
		t.Fatalf("entry.agent.Tools = %v", got)
	}
}

// TestSandbox_ConcurrencySlotReleasedOnFailure makes sure a panic /
// error path inside an engine still releases the semaphore — i.e.
// we don't leak a slot on the error branch.
func TestSandbox_ConcurrencySlotReleasedOnFailure(t *testing.T) {
	t.Parallel()
	failing := engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		return b, errdefs.Internalf("boom")
	})
	vs := spec.Spec{
		Agents:    []spec.Agent{{Name: "p"}},
		Resources: spec.Resources{MaxConcurrentRuns: 1},
	}
	c, err := New(vs, WithEngine(failing))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Stop(context.Background())
	_ = c.Launch(context.Background())

	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		_, err := c.Call(ctx, "p", agent.Request{Message: model.NewTextMessage(model.RoleUser, "x")})
		cancel()
		if err != nil {
			t.Fatalf("iteration %d: Call: %v", i, err)
		}
	}
}
