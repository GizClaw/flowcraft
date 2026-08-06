package script

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/graph"
)

// The graph-layer bridges (node/stream/parallel) are tested through
// the node's assembled env — the same object a real runtime receives.

func TestNodeBridge_ExposesNodeIdentity(t *testing.T) {
	var gotID, gotType string
	rt := fakeRuntime{exec: func(_ context.Context, _, _ string, env *agent.ScriptEnv) (*agent.ScriptSignal, error) {
		node := env.Bindings["node"].(map[string]any)
		gotID = node["id"].(func() string)()
		gotType = node["type"].(func() string)()
		return nil, nil
	}}
	reg := scriptRegistry(t, ScriptNodeDeps{Runtimes: map[string]agent.ScriptRuntime{"fake": rt}})
	g := singleScriptGraph(t, reg, ScriptConfig{Runtime: "fake", Source: "x"})
	if err := executeGraph(g, agent.NewBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotID != "n" || gotType != "script" {
		t.Fatalf("node identity = %q/%q, want n/script", gotID, gotType)
	}
}

func TestParallelBridge_FailsOpenWithoutController(t *testing.T) {
	var cancelled bool
	rt := fakeRuntime{exec: func(ctx context.Context, _, _ string, env *agent.ScriptEnv) (*agent.ScriptSignal, error) {
		parallel := env.Bindings["parallel"].(map[string]any)
		cancelled = parallel["cancelNode"].(func(string, string) bool)("other", "done")
		return nil, nil
	}}
	reg := scriptRegistry(t, ScriptNodeDeps{Runtimes: map[string]agent.ScriptRuntime{"fake": rt}})
	g := singleScriptGraph(t, reg, ScriptConfig{Runtime: "fake", Source: "x"})
	if err := executeGraph(g, agent.NewBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// A sequential run carries no fork controller; cancelNode must
	// fail open rather than error.
	if cancelled {
		t.Fatal("cancelNode returned true without a parallel controller")
	}
}

func TestParallelBridge_DelegatesToController(t *testing.T) {
	controller := &fakeParallelController{ok: true}
	rt := fakeRuntime{exec: func(ctx context.Context, _, _ string, env *agent.ScriptEnv) (*agent.ScriptSignal, error) {
		parallel := env.Bindings["parallel"].(map[string]any)
		if !parallel["cancelNode"].(func(string, string) bool)("worker", "enough") {
			t.Error("cancelNode returned false with a wired controller")
		}
		return nil, nil
	}}
	reg := scriptRegistry(t, ScriptNodeDeps{Runtimes: map[string]agent.ScriptRuntime{"fake": rt}})
	g := singleScriptGraph(t, reg, ScriptConfig{Runtime: "fake", Source: "x"})

	// Drive Execute with a controller on the context, as the kernel's
	// parallel wave would.
	ctx := graph.WithParallelController(context.Background(), controller)
	_, err := g.Execute(ctx,
		agent.Run{Identity: agent.Identity{AgentID: "test-agent", RunID: "run-1"}},
		agent.NoopHost{}, agent.NewBoard())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if controller.nodeID != "worker" || controller.reason != "enough" {
		t.Fatalf("controller saw %q/%q, want worker/enough", controller.nodeID, controller.reason)
	}
}

type fakeParallelController struct {
	ok     bool
	nodeID string
	reason string
}

func (c *fakeParallelController) CancelNode(nodeID, reason string) bool {
	c.nodeID, c.reason = nodeID, reason
	return c.ok
}

type streamAPI struct {
	subscribeNode func(raw any) (map[string]any, error)
}

type eventBusHost struct {
	agent.NoopHost
	bus event.Bus
}

func (h *eventBusHost) Publish(ctx context.Context, env event.Envelope) error {
	return h.bus.Publish(ctx, env)
}

func (h *eventBusHost) EventBus() event.Bus { return h.bus }

func streamBinding(t *testing.T, env *agent.ScriptEnv) streamAPI {
	t.Helper()
	m, ok := env.Bindings["stream"].(map[string]any)
	if !ok {
		t.Fatalf("stream binding = %T", env.Bindings["stream"])
	}
	sub, ok := m["subscribe_node"].(func(any) (map[string]any, error))
	if !ok {
		t.Fatalf("stream.subscribe_node = %T", m["subscribe_node"])
	}
	return streamAPI{subscribeNode: sub}
}

func TestStreamBridge_SubscribeReceivesNodeEvents(t *testing.T) {
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })
	host := &eventBusHost{bus: bus}

	rt := fakeRuntime{exec: func(ctx context.Context, _, _ string, env *agent.ScriptEnv) (*agent.ScriptSignal, error) {
		stream := streamBinding(t, env)
		it, err := stream.subscribeNode(map[string]any{"node_id": "n"})
		if err != nil {
			t.Errorf("subscribe_node: %v", err)
			return nil, nil
		}
		next := it["next"].(func() bool)
		current := it["current"].(func() map[string]any)

		// Publish a stream delta from this node after subscribing.
		env2, err := event.NewEnvelope(ctx, agent.SubjectStreamDelta("run-1", "test-agent.node.n"),
			agent.StreamDeltaPayload{Type: agent.StreamDeltaToken, Content: "tok"})
		if err != nil {
			t.Errorf("NewEnvelope: %v", err)
			return nil, nil
		}
		env2.SetNodeID("n")
		env2.SetHeader(event.HeaderRunID, "run-1")
		env2.SetHeader(event.HeaderAgentID, "test-agent")
		if err := host.Publish(ctx, env2); err != nil {
			t.Errorf("publish: %v", err)
			return nil, nil
		}
		if !next() {
			t.Error("next() = false, want the published delta")
			return nil, nil
		}
		cur := current()
		if cur["event"] != "stream.delta" || cur["content"] != "tok" || cur["node_id"] != "n" {
			t.Errorf("current = %v, want the delta projection", cur)
		}
		return nil, nil
	}}
	reg := scriptRegistry(t, ScriptNodeDeps{
		Runtimes: map[string]agent.ScriptRuntime{"fake": rt},
	})
	g := singleScriptGraph(t, reg, ScriptConfig{Runtime: "fake", Source: "x"})
	if err := executeGraphWithHost(g, host, agent.NewBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestStreamBridge_SubscriptionFlushedAfterExec(t *testing.T) {
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })
	host := &eventBusHost{bus: bus}

	var leakedNext func() bool
	rt := fakeRuntime{exec: func(_ context.Context, _, _ string, env *agent.ScriptEnv) (*agent.ScriptSignal, error) {
		stream := streamBinding(t, env)
		it, err := stream.subscribeNode(map[string]any{"node_id": "n"})
		if err != nil {
			t.Errorf("subscribe_node: %v", err)
			return nil, nil
		}
		leakedNext = it["next"].(func() bool)
		// The script "forgets" to close: the node's cleanup registry
		// must flush the subscription when Exec returns.
		return nil, nil
	}}
	reg := scriptRegistry(t, ScriptNodeDeps{
		Runtimes: map[string]agent.ScriptRuntime{"fake": rt},
	})
	g := singleScriptGraph(t, reg, ScriptConfig{Runtime: "fake", Source: "x"})
	if err := executeGraphWithHost(g, host, agent.NewBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if leakedNext == nil {
		t.Fatal("script never subscribed")
	}
	// The flushed subscription's channel is closed: next() returns
	// false promptly instead of blocking forever.
	done := make(chan bool, 1)
	go func() { done <- leakedNext() }()
	select {
	case v := <-done:
		if v {
			t.Fatal("next() = true on a flushed subscription")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("next() blocked after Exec returned — cleanup did not flush")
	}
}

func TestStreamBridge_NoBus(t *testing.T) {
	rt := fakeRuntime{exec: func(_ context.Context, _, _ string, env *agent.ScriptEnv) (*agent.ScriptSignal, error) {
		stream := streamBinding(t, env)
		_, err := stream.subscribeNode(map[string]any{"node_id": "n"})
		if err == nil || !errdefs.IsNotAvailable(err) {
			t.Errorf("subscribe without bus error = %v, want NotAvailable", err)
		}
		return nil, nil
	}}
	reg := scriptRegistry(t, ScriptNodeDeps{Runtimes: map[string]agent.ScriptRuntime{"fake": rt}})
	g := singleScriptGraph(t, reg, ScriptConfig{Runtime: "fake", Source: "x"})
	if err := executeGraph(g, agent.NewBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}
