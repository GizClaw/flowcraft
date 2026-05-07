package vessel

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/vessel/spec"
)

// echoEngine is a minimal engine.EngineFunc test double that turns
// every user message into an assistant echo, so vessel tests can
// assert on Result.Messages without pulling in a real engine.
func echoEngine() engine.Engine {
	return engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		main := b.Channel(engine.MainChannel)
		if len(main) == 0 {
			return b, nil
		}
		last := main[len(main)-1]
		reply := model.NewTextMessage(model.RoleAssistant, "echo: "+last.Content())
		b.AppendChannelMessage(engine.MainChannel, reply)
		return b, nil
	})
}

// taggedEngine produces an assistant message that includes the
// agent.Run id attribute, so multi-agent tests can assert which
// agent handled which request.
func taggedEngine(tag string) engine.Engine {
	return engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		b.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleAssistant, tag))
		return b, nil
	})
}

func basicSpec(name string) spec.Spec {
	return spec.Spec{
		ID:     "v-test",
		Agents: []spec.Agent{{Name: name}},
	}
}

func newTestCaptain(t *testing.T, opts ...Option) *Captain {
	t.Helper()
	all := append([]Option{WithEngine(echoEngine())}, opts...)
	c, err := New(basicSpec("primary"), all...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Stop(context.Background()) })
	return c
}

func TestCaptain_New_RequiresEngine(t *testing.T) {
	t.Parallel()
	if _, err := New(basicSpec("a")); !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation error, got %v", err)
	}
}

func TestCaptain_PendingPhase_RejectsSubmit(t *testing.T) {
	t.Parallel()
	c := newTestCaptain(t)
	if c.Phase() != PhasePending {
		t.Fatalf("phase = %s, want pending", c.Phase())
	}
	if _, err := c.Submit(context.Background(), "primary", agent.Request{}); !errdefs.IsNotAvailable(err) {
		t.Fatalf("expected NotAvailable from pre-Launch Submit, got %v", err)
	}
}

func TestCaptain_Launch_Idempotent(t *testing.T) {
	t.Parallel()
	c := newTestCaptain(t)
	if err := c.Launch(context.Background()); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if c.Phase() != PhaseRunning {
		t.Fatalf("phase = %s, want running", c.Phase())
	}
	if err := c.Launch(context.Background()); err != nil {
		t.Fatalf("re-Launch: %v", err)
	}
}

func TestCaptain_Call_HappyPath(t *testing.T) {
	t.Parallel()
	c := newTestCaptain(t)
	if err := c.Launch(context.Background()); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	res, err := c.Call(context.Background(), "primary", agent.Request{
		Message: model.NewTextMessage(model.RoleUser, "hello"),
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res == nil || res.Status != agent.StatusCompleted {
		t.Fatalf("status = %v", res)
	}
	if len(res.Messages) != 1 || res.Messages[0].Content() != "echo: hello" {
		t.Fatalf("unexpected messages: %+v", res.Messages)
	}
}

func TestCaptain_Submit_HandleWaitDelivers(t *testing.T) {
	t.Parallel()
	c := newTestCaptain(t)
	_ = c.Launch(context.Background())

	h, err := c.Submit(context.Background(), "primary", agent.Request{
		Message: model.NewTextMessage(model.RoleUser, "ping"),
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if h.RunID == "" || h.AgentName != "primary" {
		t.Fatalf("handle = %+v", h)
	}
	res, err := h.Wait(context.Background())
	if err != nil || res == nil {
		t.Fatalf("Wait: res=%v err=%v", res, err)
	}
	if res.RunID != h.RunID {
		t.Fatalf("RunID mismatch: %s vs %s", res.RunID, h.RunID)
	}
}

func TestCaptain_Submit_UnknownAgentReturnsNotFound(t *testing.T) {
	t.Parallel()
	c := newTestCaptain(t)
	_ = c.Launch(context.Background())
	if _, err := c.Submit(context.Background(), "missing", agent.Request{}); !errdefs.IsNotFound(err) {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

// TestCaptain_MultiAgent_RoutesByName confirms that a vessel with
// two foreground agents dispatches each Submit to the correct
// engine via name lookup.
func TestCaptain_MultiAgent_RoutesByName(t *testing.T) {
	t.Parallel()
	vs := spec.Spec{
		ID:     "v-multi",
		Agents: []spec.Agent{{Name: "alice"}, {Name: "bob"}},
	}
	c, err := New(vs, WithEngineFactory(func(a spec.Agent, _ Deps) (engine.Engine, error) {
		return taggedEngine("from-" + a.Name), nil
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Stop(context.Background())
	_ = c.Launch(context.Background())

	for _, name := range []string{"alice", "bob"} {
		res, err := c.Call(context.Background(), name, agent.Request{Message: model.NewTextMessage(model.RoleUser, "ping")})
		if err != nil {
			t.Fatalf("Call(%s): %v", name, err)
		}
		want := "from-" + name
		if res == nil || res.Text() != want {
			t.Fatalf("Call(%s) → %v, want %q", name, res, want)
		}
	}
}

// TestCaptain_Sidecar_BusTriggered confirms that a Sidecar agent
// runs every time an envelope matches its SubscribeTo pattern, and
// that direct Submit to the sidecar is rejected.
func TestCaptain_Sidecar_BusTriggered(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	probe := engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		hits.Add(1)
		return b, nil
	})

	vs := spec.Spec{
		ID: "v-side",
		Agents: []spec.Agent{
			{Name: "primary"},
			{Name: "moderator", Sidecar: true, SubscribeTo: "test.>"},
		},
	}
	c, err := New(vs, WithEngineFactory(func(a spec.Agent, _ Deps) (engine.Engine, error) {
		if a.Name == "moderator" {
			return probe, nil
		}
		return echoEngine(), nil
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Stop(context.Background())
	if err := c.Launch(context.Background()); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// Direct Submit to a sidecar must be rejected with Conflict.
	if _, err := c.Submit(context.Background(), "moderator", agent.Request{}); !errdefs.IsConflict(err) {
		t.Fatalf("Submit to sidecar → %v, want Conflict", err)
	}

	env, err := event.NewEnvelope(context.Background(), event.Subject("test.event"), nil)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	if err := c.Bus().Publish(context.Background(), env); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hits.Load() >= 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("sidecar did not run; hits=%d", hits.Load())
}

func TestCaptain_Drain_WaitsForInflight(t *testing.T) {
	t.Parallel()
	gate := make(chan struct{})
	slowEng := engine.EngineFunc(func(ctx context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		select {
		case <-gate:
		case <-ctx.Done():
			return b, ctx.Err()
		}
		b.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleAssistant, "done"))
		return b, nil
	})
	c, err := New(basicSpec("p"), WithEngine(slowEng))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Stop(context.Background())
	_ = c.Launch(context.Background())

	h, err := c.Submit(context.Background(), "p", agent.Request{Message: model.NewTextMessage(model.RoleUser, "hi")})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	drainErr := make(chan error, 1)
	go func() { drainErr <- c.Drain(context.Background()) }()

	time.Sleep(20 * time.Millisecond)
	if c.Phase() != PhaseDraining {
		t.Fatalf("phase = %s, want draining", c.Phase())
	}
	if _, err := c.Submit(context.Background(), "p", agent.Request{}); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Submit during Drain → %v, want NotAvailable", err)
	}

	close(gate)
	if err := <-drainErr; err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if c.Phase() != PhaseStopped {
		t.Fatalf("phase = %s, want stopped", c.Phase())
	}
	if _, err := h.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func TestCaptain_Stop_CancelsInflight(t *testing.T) {
	t.Parallel()
	hung := engine.EngineFunc(func(ctx context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		<-ctx.Done()
		return b, ctx.Err()
	})
	entered := make(chan struct{}, 1)
	hungSignal := engine.EngineFunc(func(ctx context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return b, ctx.Err()
	})
	_ = hung
	c, err := New(basicSpec("p"), WithEngine(hungSignal))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = c.Launch(context.Background())

	h, _ := c.Submit(context.Background(), "p", agent.Request{Message: model.NewTextMessage(model.RoleUser, "hi")})

	// Wait for the engine to actually enter — Stop arriving before
	// the Submit goroutine acquires would close the gate and
	// deliver (nil, NotAvailable), which is a different (also
	// valid) outcome we don't want to assert on here.
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("engine never entered")
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if err := c.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if c.Phase() != PhaseStopped {
		t.Fatalf("phase = %s, want stopped", c.Phase())
	}
	res, _ := h.Wait(context.Background())
	if res == nil || res.Status != agent.StatusCanceled {
		t.Fatalf("res = %+v, want StatusCanceled", res)
	}
}

func TestCaptain_PostStop_RejectsSubmit(t *testing.T) {
	t.Parallel()
	c := newTestCaptain(t)
	_ = c.Launch(context.Background())
	_ = c.Stop(context.Background())
	if _, err := c.Submit(context.Background(), "primary", agent.Request{}); !errdefs.IsNotAvailable(err) {
		t.Fatalf("post-Stop Submit → %v, want NotAvailable", err)
	}
}

func TestCaptain_Call_CtxCancel(t *testing.T) {
	t.Parallel()
	hung := engine.EngineFunc(func(ctx context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		<-ctx.Done()
		return b, ctx.Err()
	})
	c, err := New(basicSpec("p"), WithEngine(hung))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Stop(context.Background())
	_ = c.Launch(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	type ret struct {
		res *agent.Result
		err error
	}
	done := make(chan ret, 1)
	go func() {
		r, e := c.Call(ctx, "p", agent.Request{Message: model.NewTextMessage(model.RoleUser, "hi")})
		done <- ret{r, e}
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	got := <-done
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", got.err)
	}
}

func TestCaptain_ID_AutoGenerated(t *testing.T) {
	t.Parallel()
	vs := spec.Spec{Agents: []spec.Agent{{Name: "p"}}}
	c, err := New(vs, WithEngine(echoEngine()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Stop(context.Background())
	if !strings.HasPrefix(c.ID(), "v-") {
		t.Fatalf("auto id = %q, want v- prefix", c.ID())
	}
}

// TestCaptain_PhaseChanged_Emitted confirms the bus carries
// vessel.phase.changed envelopes for every transition.
func TestCaptain_PhaseChanged_Emitted(t *testing.T) {
	t.Parallel()
	c := newTestCaptain(t)
	sub, err := c.Bus().Subscribe(context.Background(), event.Pattern(SubjectPhaseChanged))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	got := make([]Phase, 0, 4)
	var mu sync.Mutex
	doneCh := make(chan struct{})
	go func() {
		for env := range sub.C() {
			mu.Lock()
			var p PhaseChangedPayload
			_ = env.Decode(&p)
			got = append(got, p.To)
			mu.Unlock()
			if p.To == PhaseStopped {
				close(doneCh)
				return
			}
		}
	}()

	_ = c.Launch(context.Background())
	_ = c.Stop(context.Background())

	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("phase events not delivered, got=%v", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) < 2 {
		t.Fatalf("expected at least 2 transitions, got %v", got)
	}
}
