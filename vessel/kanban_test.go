package vessel

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/history"
	"github.com/GizClaw/flowcraft/sdk/kanban"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/vessel/spec"
)

// kanbanInvokerEngine simulates a Dispatcher LLM that calls
// kanban_submit once with the user message as the worker's query,
// then waits for the callback to materialise on the next turn.
//
// The first invocation submits to targetAgent and returns a
// placeholder assistant message; the second invocation, triggered
// by the callback bridge, returns `final + " | " + lastUserText`
// so tests can assert the dispatcher reacted to the callback.
func kanbanInvokerEngine(t *testing.T, capPtr *atomic.Pointer[Captain], targetAgent string, final string) engine.Engine {
	t.Helper()
	return engine.EngineFunc(func(ctx context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		main := b.Channel(engine.MainChannel)
		if len(main) == 0 {
			return b, nil
		}
		last := main[len(main)-1].Content()

		if strings.HasPrefix(last, "[Task Callback]") {
			b.AppendChannelMessage(engine.MainChannel,
				model.NewTextMessage(model.RoleAssistant, final+" | "+last))
			return b, nil
		}

		c := capPtr.Load()
		if c == nil || c.testRegistry == nil {
			t.Errorf("captain registry unavailable")
			return b, errors.New("registry unavailable")
		}
		submit, ok := c.testRegistry.Get("kanban_submit")
		if !ok {
			t.Errorf("kanban_submit not registered")
			return b, errors.New("kanban_submit missing")
		}
		args, _ := json.Marshal(map[string]string{
			"target_agent_id": targetAgent,
			"query":           last,
			"dispatch_note":   "delegated by " + targetAgent + " test",
		})
		out, err := submit.Execute(ctx, string(args))
		if err != nil {
			b.AppendChannelMessage(engine.MainChannel,
				model.NewTextMessage(model.RoleAssistant, "submit_error: "+err.Error()))
			return b, nil
		}
		b.AppendChannelMessage(engine.MainChannel,
			model.NewTextMessage(model.RoleAssistant, "dispatched: "+out))
		return b, nil
	})
}

// dispatcherSpec returns a spec.Spec wiring a Dispatcher +
// Worker pair on the kanban board, with shared history so the
// callback bridge has a transcript to append to.
func dispatcherSpec() spec.Spec {
	return spec.Spec{
		ID: "v-kanban-test",
		Agents: []spec.Agent{
			{Name: "boss", Dispatcher: true, HistoryAccess: spec.HistoryAccessReadWrite},
			{Name: "worker", HistoryAccess: spec.HistoryAccessReadWrite},
		},
		History: &spec.History{Kind: "buffer", MaxMessages: 50},
		Kanban:  &spec.Kanban{MaxPendingTasks: 16, MaxProducerChain: 3},
	}
}

func TestKanban_DispatcherWorker_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var capPtr atomic.Pointer[Captain]

	c, err := New(
		dispatcherSpec(),
		WithEngineFactory(func(aspec spec.Agent, _ Deps) (engine.Engine, error) {
			switch aspec.Name {
			case "boss":
				return kanbanInvokerEngine(t, &capPtr, "worker", "boss-final"), nil
			case "worker":
				return engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
					main := b.Channel(engine.MainChannel)
					last := ""
					if len(main) > 0 {
						last = main[len(main)-1].Content()
					}
					b.AppendChannelMessage(engine.MainChannel,
						model.NewTextMessage(model.RoleAssistant, "worker-result for: "+last))
					return b, nil
				}), nil
			}
			return nil, errors.New("no engine for " + aspec.Name)
		}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Stop(context.Background()) }()
	capPtr.Store(c)

	if err := c.Launch(ctx); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	res, err := c.Call(ctx, "boss", agent.Request{
		ContextID: "conv-1",
		Message:   model.NewTextMessage(model.RoleUser, "draft a CHANGELOG"),
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(res.Messages[0].Content(), "dispatched:") {
		t.Fatalf("first turn output unexpected: %q", res.Messages[0].Content())
	}

	// Wait for: worker runs → callback bridge appends → boss
	// re-Submit completes a second turn that begins with "boss-final".
	if !waitForHistory(t, c, "conv-1", "boss-final |", 3*time.Second) {
		t.Fatalf("history did not gain a callback turn")
	}
}

func TestKanban_LoopGuard_RejectsBeyondCap(t *testing.T) {
	t.Parallel()
	vs := spec.Spec{
		ID: "v-loop",
		Agents: []spec.Agent{
			{Name: "boss", Dispatcher: true},
			{Name: "leaf", ProducerChain: 1}, // unused but documents intent
		},
		Kanban: &spec.Kanban{MaxProducerChain: 1},
	}

	c, err := New(vs, WithEngine(echoEngine()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Stop(context.Background()) }()
	_ = c.Launch(context.Background())

	// Stamp the same ctx values [Captain.dispatch] would set in
	// production: chain depth (loop guard), context id (history
	// routing), captain pointer + dispatcher name (the
	// vessel-aware tool's identity, post the ctx-based refactor).
	ctx := withChainDepth(context.Background(), 2)
	ctx = withContextID(ctx, "conv-loop")
	ctx = withCaptain(ctx, c)
	ctx = withDispatcher(ctx, "boss")
	tool := vesselSubmitTool{}
	args, _ := json.Marshal(map[string]string{"target_agent_id": "leaf", "query": "x"})
	if _, err := tool.Execute(ctx, string(args)); !errdefs.IsConflict(err) {
		t.Fatalf("expected Conflict from chain-cap rejection, got %v", err)
	}
}

func TestKanban_LoopGuard_RejectsSelfDispatch(t *testing.T) {
	t.Parallel()
	vs := spec.Spec{
		ID: "v-self",
		Agents: []spec.Agent{
			{Name: "boss", Dispatcher: true},
		},
		Kanban: &spec.Kanban{},
	}
	c, err := New(vs, WithEngine(echoEngine()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Stop(context.Background()) }()
	if err := c.Launch(context.Background()); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	tool := vesselSubmitTool{}
	ctx := withCaptain(context.Background(), c)
	ctx = withDispatcher(ctx, "boss")
	args, _ := json.Marshal(map[string]string{"target_agent_id": "boss", "query": "self"})
	if _, err := tool.Execute(ctx, string(args)); !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation error rejecting self-dispatch, got %v", err)
	}
}

func TestKanban_DispatcherTools_Registered(t *testing.T) {
	t.Parallel()
	c, err := New(
		dispatcherSpec(),
		WithEngineFactory(func(_ spec.Agent, _ Deps) (engine.Engine, error) {
			return engine.EngineFunc(nil), nil
		}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Stop(context.Background()) }()

	reg := c.testRegistry
	if reg == nil {
		t.Fatal("captain has no tool registry")
	}
	for _, name := range []string{"kanban_submit", "task_context"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("tool %q not registered", name)
		}
	}

	bossEntry := c.entries["boss"]
	hasSubmit := false
	hasCtx := false
	for _, id := range bossEntry.agent.Tools {
		if id == "kanban_submit" {
			hasSubmit = true
		}
		if id == "task_context" {
			hasCtx = true
		}
	}
	if !hasSubmit || !hasCtx {
		t.Errorf("boss agent.Tools missing kanban ids: %v", bossEntry.agent.Tools)
	}
}

func TestKanban_FailedTask_DeliversCallback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var capPtr atomic.Pointer[Captain]

	vs := dispatcherSpec()
	c, err := New(
		vs,
		WithEngineFactory(func(aspec spec.Agent, _ Deps) (engine.Engine, error) {
			switch aspec.Name {
			case "boss":
				return kanbanInvokerEngine(t, &capPtr, "worker", "boss-after-failure"), nil
			case "worker":
				return engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
					return b, errors.New("worker exploded on purpose")
				}), nil
			}
			return nil, errors.New("no engine")
		}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Stop(context.Background()) }()
	capPtr.Store(c)
	_ = c.Launch(ctx)

	res, err := c.Call(ctx, "boss", agent.Request{
		ContextID: "conv-fail",
		Message:   model.NewTextMessage(model.RoleUser, "do impossible thing"),
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(res.Messages[0].Content(), "dispatched:") {
		t.Fatalf("first turn unexpected: %q", res.Messages[0].Content())
	}

	if !waitForHistory(t, c, "conv-fail", "Status: failed", 2*time.Second) {
		t.Fatal("failure callback never appended to history")
	}
}

// waitForHistory polls c.history for the given conversation until a
// substring shows up in any message text or the timeout fires. Used
// to wait for asynchronous callback bridge work to materialise.
func waitForHistory(t *testing.T, c *Captain, contextID, want string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		msgs, _ := c.history.Load(context.Background(), contextID, history.Budget{})
		for _, m := range msgs {
			if strings.Contains(m.Content(), want) {
				return true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// silence unused imports kept for parallel test scaffolding.
var (
	_ = sync.Once{}
	_ = kanban.HeaderCardID
	_ = (*tool.Registry)(nil)
)

// TestKanban_MultiDispatcher_NoNameCollision asserts the registry
// holds exactly one stateless wrapper per kanban tool, regardless
// of how many Dispatchers the vessel declares. The wrapper is
// captain-/dispatcher-agnostic; correctness comes from the ctx,
// not from per-Dispatcher copies in the registry.
func TestKanban_MultiDispatcher_NoNameCollision(t *testing.T) {
	t.Parallel()

	vs := spec.Spec{
		ID: "v-md",
		Agents: []spec.Agent{
			{Name: "boss-a", Dispatcher: true, HistoryAccess: spec.HistoryAccessReadWrite},
			{Name: "boss-b", Dispatcher: true, HistoryAccess: spec.HistoryAccessReadWrite},
			{Name: "worker", HistoryAccess: spec.HistoryAccessReadWrite},
		},
		History: &spec.History{Kind: "buffer", MaxMessages: 50},
		Kanban:  &spec.Kanban{MaxPendingTasks: 8, MaxProducerChain: 3},
	}
	c, err := New(vs, WithEngine(echoEngine()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Stop(context.Background()) }()

	// Both kanban tools must be present and the wrappers are the
	// stateless type — multi-Dispatcher uniqueness is achieved via
	// ctx, not via per-Dispatcher registry entries.
	for _, name := range []string{"kanban_submit", "task_context"} {
		tl, ok := c.testRegistry.Get(name)
		if !ok {
			t.Fatalf("registry missing %q", name)
		}
		switch name {
		case "kanban_submit":
			if _, ok := tl.(vesselSubmitTool); !ok {
				t.Errorf("kanban_submit wrapper has unexpected type %T (want vesselSubmitTool)", tl)
			}
		case "task_context":
			if _, ok := tl.(vesselTaskContextTool); !ok {
				t.Errorf("task_context wrapper has unexpected type %T (want vesselTaskContextTool)", tl)
			}
		}
	}

	// Both Dispatchers must have BOTH kanban tool ids in their
	// effective allow-list (post-augmentation), so each LLM gets
	// the same uniform tool surface.
	for _, name := range []string{"boss-a", "boss-b"} {
		entry := c.entries[name]
		got := map[string]bool{}
		for _, id := range entry.agent.Tools {
			got[id] = true
		}
		if !got["kanban_submit"] || !got["task_context"] {
			t.Errorf("%s.agent.Tools missing kanban ids: %v", name, entry.agent.Tools)
		}
	}
}

// TestKanban_MultiDispatcher_CallbackRouting is the behavioural
// counterpart: dispatching from boss-a routes the worker callback
// back to boss-a (NOT boss-b). Pre-fix this failed because the
// last-registered Dispatcher's wrapper was the only one in the
// registry, so the recorded origin always pointed at it.
func TestKanban_MultiDispatcher_CallbackRouting(t *testing.T) {
	t.Parallel()

	var capPtr atomic.Pointer[Captain]
	var bossASubmits, bossBSubmits atomic.Int32

	bossEngine := func(name string, counter *atomic.Int32) engine.Engine {
		return engine.EngineFunc(func(ctx context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
			counter.Add(1)
			main := b.Channel(engine.MainChannel)
			if len(main) == 0 {
				return b, nil
			}
			last := main[len(main)-1].Content()
			if strings.HasPrefix(last, "[Task Callback]") {
				b.AppendChannelMessage(engine.MainChannel,
					model.NewTextMessage(model.RoleAssistant, name+":observed-callback"))
				return b, nil
			}
			c := capPtr.Load()
			if c == nil {
				return b, nil
			}
			submit, ok := c.testRegistry.Get("kanban_submit")
			if !ok {
				return b, nil
			}
			args, _ := json.Marshal(map[string]string{
				"target_agent_id": "worker",
				"query":           name + " asks worker to do thing",
			})
			if _, err := submit.Execute(ctx, string(args)); err != nil {
				return b, err
			}
			b.AppendChannelMessage(engine.MainChannel,
				model.NewTextMessage(model.RoleAssistant, name+":dispatched"))
			return b, nil
		})
	}

	vs := spec.Spec{
		ID: "v-md-cb",
		Agents: []spec.Agent{
			{Name: "boss-a", Dispatcher: true, HistoryAccess: spec.HistoryAccessReadWrite},
			{Name: "boss-b", Dispatcher: true, HistoryAccess: spec.HistoryAccessReadWrite},
			{Name: "worker", HistoryAccess: spec.HistoryAccessReadWrite},
		},
		History: &spec.History{Kind: "buffer", MaxMessages: 50},
		Kanban:  &spec.Kanban{MaxPendingTasks: 4, MaxProducerChain: 2},
	}
	c, err := New(
		vs,
		WithEngineFactory(func(aspec spec.Agent, _ Deps) (engine.Engine, error) {
			switch aspec.Name {
			case "boss-a":
				return bossEngine("boss-a", &bossASubmits), nil
			case "boss-b":
				return bossEngine("boss-b", &bossBSubmits), nil
			case "worker":
				return engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
					b.AppendChannelMessage(engine.MainChannel,
						model.NewTextMessage(model.RoleAssistant, "worker-out"))
					return b, nil
				}), nil
			}
			return nil, nil
		}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Stop(context.Background()) }()
	capPtr.Store(c)
	if err := c.Launch(context.Background()); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if _, err := c.Call(context.Background(), "boss-a", agent.Request{
		ContextID: "conv-a",
		Message:   model.NewTextMessage(model.RoleUser, "kick"),
	}); err != nil {
		t.Fatalf("Call boss-a: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bossASubmits.Load() >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if bossASubmits.Load() < 2 {
		t.Fatalf("boss-a never observed callback turn (calls=%d)", bossASubmits.Load())
	}
	if bossBSubmits.Load() != 0 {
		t.Fatalf("boss-b was invoked %d times — callback routed to wrong Dispatcher", bossBSubmits.Load())
	}
}

// TestKanban_DrainWaitsForKanbanWorker pins gap #8: a slow
// kanban-spawned worker MUST keep the Captain in PhaseDraining
// until it finishes. Pre-fix Drain returned almost instantly
// because the foreground inflight WG was already 0 — the
// kanban-owned dispatch goroutine was untracked.
func TestKanban_DrainWaitsForKanbanWorker(t *testing.T) {
	t.Parallel()

	var capPtr atomic.Pointer[Captain]
	workerStarted := make(chan struct{})
	var workerStartOnce atomic.Bool
	var workerCompleted atomic.Bool

	bossEngine := engine.EngineFunc(func(ctx context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		c := capPtr.Load()
		submit, _ := c.testRegistry.Get("kanban_submit")
		args, _ := json.Marshal(map[string]string{
			"target_agent_id": "worker",
			"query":           "slow task",
		})
		_, _ = submit.Execute(ctx, string(args))
		b.AppendChannelMessage(engine.MainChannel,
			model.NewTextMessage(model.RoleAssistant, "dispatched"))
		return b, nil
	})

	workerEngine := engine.EngineFunc(func(ctx context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		if workerStartOnce.CompareAndSwap(false, true) {
			close(workerStarted)
		}
		select {
		case <-time.After(500 * time.Millisecond):
			workerCompleted.Store(true)
		case <-ctx.Done():
		}
		b.AppendChannelMessage(engine.MainChannel,
			model.NewTextMessage(model.RoleAssistant, "worker-done"))
		return b, nil
	})

	vs := spec.Spec{
		ID: "v-drain",
		Agents: []spec.Agent{
			{Name: "boss", Dispatcher: true, HistoryAccess: spec.HistoryAccessReadWrite},
			{Name: "worker", HistoryAccess: spec.HistoryAccessReadWrite},
		},
		History: &spec.History{Kind: "buffer", MaxMessages: 20},
		Kanban:  &spec.Kanban{MaxPendingTasks: 4, MaxProducerChain: 2},
	}
	c, err := New(vs, WithEngineFactory(func(aspec spec.Agent, _ Deps) (engine.Engine, error) {
		if aspec.Name == "worker" {
			return workerEngine, nil
		}
		return bossEngine, nil
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Stop(context.Background()) }()
	capPtr.Store(c)
	if err := c.Launch(context.Background()); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if _, err := c.Call(context.Background(), "boss", agent.Request{
		ContextID: "conv",
		Message:   model.NewTextMessage(model.RoleUser, "go"),
	}); err != nil {
		t.Fatalf("Call boss: %v", err)
	}
	select {
	case <-workerStarted:
	case <-time.After(time.Second):
		t.Fatal("worker never started")
	}

	// Drain with a generous budget — the worker takes 500ms; if
	// Drain correctly waits, total elapsed will be ~500ms and the
	// worker MUST have completed. Pre-fix elapsed was sub-100ms
	// and workerCompleted was still false.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer drainCancel()
	start := time.Now()
	if err := c.Drain(drainCtx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	elapsed := time.Since(start)
	if !workerCompleted.Load() {
		t.Fatalf("Drain returned (after %s) while worker still in flight — kanban dispatch not tracked by inflight WG", elapsed)
	}
	if elapsed < 400*time.Millisecond {
		t.Fatalf("Drain returned in %s — too quick to have actually waited for the 500ms worker", elapsed)
	}
}
