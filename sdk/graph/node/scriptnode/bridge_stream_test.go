package scriptnode

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/graph"
	nodepkg "github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/script/jsrt"
)

func TestStreamBridge_SubscribeNode_NoEventBusIsNotAvailable(t *testing.T) {
	subscribe := streamSubscribeFunc(t, "run-1", nil)

	_, err := subscribe(map[string]any{"node_id": "planner"})
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("subscribe_node error = %v, want NotAvailable", err)
	}
}

func TestStreamBridge_SubscribeNode_FiltersNodeAndProjectsCurrent(t *testing.T) {
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })

	subscribe := streamSubscribeFunc(t, "run-1", bus)
	iter, err := subscribe(map[string]any{"node_id": "planner"})
	if err != nil {
		t.Fatalf("subscribe_node: %v", err)
	}
	defer iter["close"].(func() error)()

	publishStreamDelta(t, bus, "run-1", "other", "agent-a", "ignore me")
	publishStreamDelta(t, bus, "run-1", "planner", "agent-a", "keep me")

	if !iter["next"].(func() bool)() {
		t.Fatal("next() = false, want matching delta")
	}
	cur := iter["current"].(func() map[string]any)()
	checkCurrentDelta(t, cur, map[string]any{
		"type":     "token",
		"content":  "keep me",
		"run_id":   "run-1",
		"node_id":  "planner",
		"agent_id": "agent-a",
	})
	if cur["subject"] == "" {
		t.Fatalf("current.subject missing: %+v", cur)
	}
	if cur["id"] == "" || cur["envelope_id"] == "" {
		t.Fatalf("current envelope id missing: %+v", cur)
	}
	if cur["time"] == "" {
		t.Fatalf("current.time missing: %+v", cur)
	}
}

func TestStreamBridge_SubscribeNode_RunIDOverrideRejected(t *testing.T) {
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })

	subscribe := streamSubscribeFunc(t, "run-default", bus)
	_, err := subscribe(map[string]any{"run_id": "run-override", "node_id": "planner"})
	if !errdefs.IsValidation(err) {
		t.Fatalf("subscribe_node error = %v, want Validation", err)
	}
}

func TestStreamBridge_SubscribeNode_CurrentPreservesPayloadObjectFields(t *testing.T) {
	tests := []struct {
		name    string
		payload map[string]any
		want    map[string]any
	}{
		{
			name:    "custom progress field",
			payload: map[string]any{"type": "progress", "pct": 0.5},
			want:    map[string]any{"type": "progress", "pct": 0.5},
		},
		{
			name:    "bare emit payload field",
			payload: map[string]any{"type": "token", "payload": "x"},
			want:    map[string]any{"type": "token", "payload": "x"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bus := event.NewMemoryBus()
			t.Cleanup(func() { _ = bus.Close() })

			subscribe := streamSubscribeFunc(t, "run-1", bus)
			iter, err := subscribe(map[string]any{"node_id": "planner"})
			if err != nil {
				t.Fatalf("subscribe_node: %v", err)
			}
			defer iter["close"].(func() error)()

			publishStreamDeltaPayload(t, bus, "run-1", "planner", "agent-a", tt.payload)
			if !iter["next"].(func() bool)() {
				t.Fatal("next() = false, want matching delta")
			}
			cur := iter["current"].(func() map[string]any)()
			checkCurrentDelta(t, cur, tt.want)
		})
	}
}

func TestStreamBridge_SubscribeNode_SeesLifecycleDeltaEndedInOrder(t *testing.T) {
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })

	subscribe := streamSubscribeFunc(t, "run-1", bus)
	iter, err := subscribe(map[string]any{"node_id": "planner"})
	if err != nil {
		t.Fatalf("subscribe_node: %v", err)
	}
	defer iter["close"].(func() error)()

	publishStepLifecycle(t, bus, "run-1", "planner", "agent-a", "start", nil)
	publishStreamDelta(t, bus, "run-1", "planner", "agent-a", "hello")
	publishStepLifecycle(t, bus, "run-1", "planner", "agent-a", "complete", map[string]any{
		"iteration": 1,
	})

	if !iter["next"].(func() bool)() {
		t.Fatal("next() = false, want step.started")
	}
	checkCurrentEvent(t, iter["current"].(func() map[string]any)(), map[string]any{
		"event":    "step.started",
		"run_id":   "run-1",
		"node_id":  "planner",
		"agent_id": "agent-a",
	})

	if !iter["next"].(func() bool)() {
		t.Fatal("next() = false, want stream.delta")
	}
	checkCurrentDelta(t, iter["current"].(func() map[string]any)(), map[string]any{
		"type":    "token",
		"content": "hello",
	})

	if !iter["next"].(func() bool)() {
		t.Fatal("next() = false, want step.ended")
	}
	cur := iter["current"].(func() map[string]any)()
	checkCurrentEvent(t, cur, map[string]any{
		"event":     "step.ended",
		"status":    "success",
		"iteration": float64(1),
		"run_id":    "run-1",
		"node_id":   "planner",
		"agent_id":  "agent-a",
	})
	if iter["next"].(func() bool)() {
		t.Fatal("next() after step.ended = true, want false")
	}
}

func TestStreamBridge_SubscribeNode_StepErrorFoldsToEndedAndCloses(t *testing.T) {
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })

	subscribe := streamSubscribeFunc(t, "run-1", bus)
	iter, err := subscribe(map[string]any{"node_id": "planner"})
	if err != nil {
		t.Fatalf("subscribe_node: %v", err)
	}
	defer iter["close"].(func() error)()

	publishStepLifecycle(t, bus, "run-1", "planner", "agent-a", "error", map[string]any{
		"error": "boom",
	})

	if !iter["next"].(func() bool)() {
		t.Fatal("next() = false, want step.error as step.ended")
	}
	checkCurrentEvent(t, iter["current"].(func() map[string]any)(), map[string]any{
		"event":  "step.ended",
		"status": "error",
		"error":  "boom",
	})
	if iter["next"].(func() bool)() {
		t.Fatal("next() after step.error = true, want false")
	}
}

func TestStreamBridge_SubscribeNode_StepSkippedIsTerminal(t *testing.T) {
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })

	subscribe := streamSubscribeFunc(t, "run-1", bus)
	iter, err := subscribe(map[string]any{"node_id": "planner"})
	if err != nil {
		t.Fatalf("subscribe_node: %v", err)
	}
	defer iter["close"].(func() error)()

	publishStepLifecycle(t, bus, "run-1", "planner", "agent-a", "skipped", nil)

	if !iter["next"].(func() bool)() {
		t.Fatal("next() = false, want step.skipped")
	}
	checkCurrentEvent(t, iter["current"].(func() map[string]any)(), map[string]any{
		"event":    "step.skipped",
		"status":   "skipped",
		"run_id":   "run-1",
		"node_id":  "planner",
		"agent_id": "agent-a",
	})
	if iter["next"].(func() bool)() {
		t.Fatal("next() after step.skipped = true, want false")
	}
}

func TestStreamBridge_SubscribeNode_FiltersOtherNodeLifecycleEvents(t *testing.T) {
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })

	subscribe := streamSubscribeFunc(t, "run-1", bus)
	iter, err := subscribe(map[string]any{"node_id": "planner"})
	if err != nil {
		t.Fatalf("subscribe_node: %v", err)
	}
	defer iter["close"].(func() error)()

	publishStepLifecycle(t, bus, "run-1", "other", "agent-a", "skipped", nil)
	publishStepLifecycle(t, bus, "run-1", "planner", "agent-a", "start", nil)

	if !iter["next"].(func() bool)() {
		t.Fatal("next() = false, want target node step.started")
	}
	checkCurrentEvent(t, iter["current"].(func() map[string]any)(), map[string]any{
		"event":   "step.started",
		"node_id": "planner",
	})
}

func TestStreamBridge_SubscribeNode_NextReturnsFalseWhenCallContextCanceled(t *testing.T) {
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	subscribe := streamSubscribeFuncWithContext(t, ctx, "run-1", bus)
	iter, err := subscribe(map[string]any{"node_id": "planner"})
	if err != nil {
		t.Fatalf("subscribe_node: %v", err)
	}
	defer iter["close"].(func() error)()

	started := make(chan struct{})
	done := make(chan bool, 1)
	go func() {
		close(started)
		done <- iter["next"].(func() bool)()
	}()

	<-started
	cancel()

	select {
	case got := <-done:
		if got {
			t.Fatal("next() after call context cancel = true, want false")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("next() did not unblock after call context cancel")
	}
}

func TestStreamBridge_SubscribeNode_CloseMakesNextFalse(t *testing.T) {
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })

	subscribe := streamSubscribeFunc(t, "run-1", bus)
	iter, err := subscribe(map[string]any{"node_id": "planner"})
	if err != nil {
		t.Fatalf("subscribe_node: %v", err)
	}
	if err := iter["close"].(func() error)(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if iter["next"].(func() bool)() {
		t.Fatal("next() after close = true, want false")
	}
}

func TestStreamBridge_SubscribeNode_FullBufferPreservesTerminalEvent(t *testing.T) {
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })

	subscribe := streamSubscribeFunc(t, "run-1", bus)
	iter, err := subscribe(map[string]any{"node_id": "planner", "buffer_size": 1})
	if err != nil {
		t.Fatalf("subscribe_node: %v", err)
	}
	defer iter["close"].(func() error)()

	envs := []event.Envelope{
		newStreamDeltaEnvelope(t, "run-1", "planner", "agent-a", "first"),
		newStreamDeltaEnvelope(t, "run-1", "planner", "agent-a", "second"),
		newStepLifecycleEnvelope(t, "run-1", "planner", "agent-a", "complete", nil),
	}
	done := make(chan error, 1)
	go func() {
		for _, env := range envs {
			if err := bus.Publish(context.Background(), env); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("publish: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("publish blocked with full node subscription buffer")
	}

	if !iter["next"].(func() bool)() {
		t.Fatal("next() = false, want terminal event preserved")
	}
	cur := iter["current"].(func() map[string]any)()
	checkCurrentEvent(t, cur, map[string]any{
		"event":  "step.ended",
		"status": "success",
	})
	if iter["next"].(func() bool)() {
		t.Fatal("next() after preserved terminal event = true, want false")
	}
}

func TestStreamBridge_SubscribeNode_InvalidBufferSize(t *testing.T) {
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })

	subscribe := streamSubscribeFunc(t, "run-1", bus)
	tests := []struct {
		name  string
		value any
	}{
		{name: "zero", value: 0},
		{name: "negative", value: -1},
		{name: "non number", value: "1"},
		{name: "fractional", value: 1.5},
		{name: "too large", value: maxStreamSubBufferSize + 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := subscribe(map[string]any{"node_id": "planner", "buffer_size": tt.value})
			if !errdefs.IsValidation(err) {
				t.Fatalf("subscribe_node error = %v, want Validation", err)
			}
		})
	}
}

func TestScriptNode_StreamBridge_SubscribeNodeFromScript(t *testing.T) {
	mem := event.NewMemoryBus()
	t.Cleanup(func() { _ = mem.Close() })
	bus := &subscribeNotifyBus{
		Bus:        mem,
		subscribed: make(chan struct{}),
	}

	rt := jsrt.New(jsrt.WithPoolSize(1))
	factory := nodepkg.NewFactory()
	Register(factory, Deps{ScriptRuntime: rt, EventBus: bus})
	n, err := factory.Build(graph.NodeDefinition{
		ID:   "listener",
		Type: "script",
		Config: map[string]any{
			"source": `
				var sub = stream.subscribe_node({ node_id: "planner" });
				if (!sub.next()) throw new Error("expected a stream delta");
				var d = sub.current();
				board.setVar("stream_content", d.content);
				board.setVar("stream_run_id", d.run_id);
				board.setVar("stream_node_id", d.node_id);
				board.setVar("stream_agent_id", d.agent_id);
				board.setVar("stream_subject", d.subject);
				board.setVar("stream_has_envelope_id", !!d.envelope_id);
				sub.close();
			`,
		},
	})
	if err != nil {
		t.Fatalf("build script node: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	board := graph.NewBoard()
	done := make(chan error, 1)
	go func() {
		done <- n.ExecuteBoard(graph.ExecutionContext{Context: ctx, RunID: "run-js"}, board)
	}()

	select {
	case <-bus.subscribed:
	case <-ctx.Done():
		t.Fatalf("script did not subscribe: %v", ctx.Err())
	}

	publishStreamDelta(t, mem, "run-js", "other", "agent-js", "ignore me")
	publishStreamDelta(t, mem, "run-js", "planner", "agent-js", "hello script")

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ExecuteBoard: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("ExecuteBoard did not finish: %v", ctx.Err())
	}

	boardChecks := map[string]any{
		"stream_content":         "hello script",
		"stream_run_id":          "run-js",
		"stream_node_id":         "planner",
		"stream_agent_id":        "agent-js",
		"stream_has_envelope_id": true,
	}
	for key, want := range boardChecks {
		got, _ := board.GetVar(key)
		if got != want {
			t.Fatalf("%s = %v, want %v", key, got, want)
		}
	}
	if got, _ := board.GetVar("stream_subject"); got == "" {
		t.Fatal("stream_subject missing")
	}
}

func TestScriptNode_StreamBridge_CleansUpUnclosedSubscriptions(t *testing.T) {
	tests := []struct {
		name    string
		source  string
		wantErr bool
	}{
		{
			name:   "natural return",
			source: `stream.subscribe_node({ node_id: "planner" });`,
		},
		{
			name:    "throw",
			source:  `stream.subscribe_node({ node_id: "planner" }); throw new Error("boom");`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mem := event.NewMemoryBus()
			t.Cleanup(func() { _ = mem.Close() })
			bus := &closeTrackingBus{Bus: mem}

			rt := jsrt.New(jsrt.WithPoolSize(1))
			n := New("listener", "script", tt.source, nil, rt)
			n.eventBus = bus

			err := n.ExecuteBoard(graph.ExecutionContext{
				Context: context.Background(),
				RunID:   "run-cleanup",
			}, graph.NewBoard())
			if tt.wantErr && err == nil {
				t.Fatal("expected script error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ExecuteBoard: %v", err)
			}
			if bus.closeCount() != 1 {
				t.Fatalf("subscription close count = %d, want 1", bus.closeCount())
			}
		})
	}
}

func streamSubscribeFunc(t *testing.T, runID string, bus event.Bus) func(any) (map[string]any, error) {
	t.Helper()
	return streamSubscribeFuncWithContext(t, context.Background(), runID, bus)
}

func streamSubscribeFuncWithContext(t *testing.T, ctx context.Context, runID string, bus event.Bus) func(any) (map[string]any, error) {
	t.Helper()
	_, raw := newStreamBridge(runID, bus)(ctx)
	api, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("stream bridge shape = %T", raw)
	}
	subscribe, ok := api["subscribe_node"].(func(any) (map[string]any, error))
	if !ok {
		t.Fatalf("subscribe_node shape = %T", api["subscribe_node"])
	}
	return subscribe
}

func publishStreamDelta(t *testing.T, bus event.Bus, runID, nodeID, agentID, content string) {
	t.Helper()
	publishStreamDeltaPayload(t, bus, runID, nodeID, agentID, engine.StreamDeltaPayload{
		Type:    engine.StreamDeltaToken,
		Content: content,
	})
}

func publishStreamDeltaPayload(t *testing.T, bus event.Bus, runID, nodeID, agentID string, payload any) {
	t.Helper()
	env := newStreamDeltaPayloadEnvelope(t, runID, nodeID, agentID, payload)
	if err := bus.Publish(context.Background(), env); err != nil {
		t.Fatalf("publish %s: %v", nodeID, err)
	}
}

func newStreamDeltaEnvelope(t *testing.T, runID, nodeID, agentID, content string) event.Envelope {
	t.Helper()
	return newStreamDeltaPayloadEnvelope(t, runID, nodeID, agentID, engine.StreamDeltaPayload{
		Type:    engine.StreamDeltaToken,
		Content: content,
	})
}

func newStreamDeltaPayloadEnvelope(t *testing.T, runID, nodeID, agentID string, payload any) event.Envelope {
	t.Helper()
	subject := engine.SubjectStreamDelta(runID, agentID+".node."+nodeID)
	env, err := event.NewEnvelope(context.Background(), subject, payload)
	if err != nil {
		t.Fatalf("new envelope: %v", err)
	}
	env.SetRunID(runID)
	env.SetNodeID(nodeID)
	env.SetAgentID(agentID)
	return env
}

func publishStepLifecycle(t *testing.T, bus event.Bus, runID, nodeID, agentID, kind string, payload any) {
	t.Helper()
	env := newStepLifecycleEnvelope(t, runID, nodeID, agentID, kind, payload)
	if err := bus.Publish(context.Background(), env); err != nil {
		t.Fatalf("publish %s %s: %v", nodeID, kind, err)
	}
}

func newStepLifecycleEnvelope(t *testing.T, runID, nodeID, agentID, kind string, payload any) event.Envelope {
	t.Helper()
	stepActor := agentID + ".node." + nodeID
	var subject event.Subject
	switch kind {
	case "start":
		subject = engine.SubjectStepStart(runID, stepActor)
	case "complete":
		subject = engine.SubjectStepComplete(runID, stepActor)
	case "error":
		subject = engine.SubjectStepError(runID, stepActor)
	case "skipped":
		subject = event.Subject(engine.SubjectPrefix + engine.SanitiseID(runID) + ".step." + engine.SanitiseID(stepActor) + ".skipped")
	default:
		t.Fatalf("unknown lifecycle kind %q", kind)
	}
	env, err := event.NewEnvelope(context.Background(), subject, payload)
	if err != nil {
		t.Fatalf("new lifecycle envelope: %v", err)
	}
	env.SetRunID(runID)
	env.SetNodeID(nodeID)
	env.SetAgentID(agentID)
	return env
}

func checkCurrentDelta(t *testing.T, cur map[string]any, want map[string]any) {
	t.Helper()
	checkCurrentEvent(t, cur, map[string]any{"event": "stream.delta"})
	for key, expected := range want {
		if got := cur[key]; got != expected {
			t.Fatalf("current.%s = %v, want %v (current=%+v)", key, got, expected, cur)
		}
	}
}

func checkCurrentEvent(t *testing.T, cur map[string]any, want map[string]any) {
	t.Helper()
	checkCurrentMetadata(t, cur)
	for key, expected := range want {
		if got := cur[key]; got != expected {
			t.Fatalf("current.%s = %v, want %v (current=%+v)", key, got, expected, cur)
		}
	}
}

func checkCurrentMetadata(t *testing.T, cur map[string]any) {
	t.Helper()
	for _, key := range []string{"event", "run_id", "node_id", "agent_id", "subject", "envelope_id", "time"} {
		v, ok := cur[key]
		if !ok || v == "" {
			t.Fatalf("current.%s missing: %+v", key, cur)
		}
	}
}

type subscribeNotifyBus struct {
	event.Bus
	subscribed chan struct{}
	once       sync.Once
}

func (b *subscribeNotifyBus) Subscribe(ctx context.Context, pattern event.Pattern, opts ...event.SubOption) (event.Subscription, error) {
	sub, err := b.Bus.Subscribe(ctx, pattern, opts...)
	if err == nil {
		b.once.Do(func() { close(b.subscribed) })
	}
	return sub, err
}

type closeTrackingBus struct {
	event.Bus
	mu     sync.Mutex
	closed int
}

func (b *closeTrackingBus) Subscribe(ctx context.Context, pattern event.Pattern, opts ...event.SubOption) (event.Subscription, error) {
	sub, err := b.Bus.Subscribe(ctx, pattern, opts...)
	if err != nil {
		return nil, err
	}
	return &closeTrackingSubscription{Subscription: sub, bus: b}, nil
}

func (b *closeTrackingBus) closeCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

type closeTrackingSubscription struct {
	event.Subscription
	bus  *closeTrackingBus
	once sync.Once
}

func (s *closeTrackingSubscription) Close() error {
	err := s.Subscription.Close()
	s.once.Do(func() {
		s.bus.mu.Lock()
		s.bus.closed++
		s.bus.mu.Unlock()
	})
	return err
}
