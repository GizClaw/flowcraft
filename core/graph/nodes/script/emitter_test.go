package script

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/message"
)

func TestStringifyPayload(t *testing.T) {
	if got := stringifyPayload("hello"); got != "hello" {
		t.Fatalf("stringifyPayload(string) = %q", got)
	}
	if got := stringifyPayload(map[string]any{"a": 1}); got != `{"a":1}` {
		t.Fatalf("stringifyPayload(map) = %q", got)
	}
}

func TestPayloadToToolCall(t *testing.T) {
	call, ok := payloadToToolCall(map[string]any{
		"id": "c1", "name": "search", "arguments": map[string]any{"q": "go"},
	})
	if !ok {
		t.Fatal("valid tool_call payload rejected")
	}
	if call.ID != "c1" || call.Name != "search" || string(call.Arguments) != `{"q":"go"}` {
		t.Fatalf("call = %+v", call)
	}
	if _, ok := payloadToToolCall(map[string]any{"name": "search"}); ok {
		t.Fatal("missing id must be rejected")
	}
}

func TestPayloadToToolResult(t *testing.T) {
	result, ok := payloadToToolResult(map[string]any{
		"tool_call_id": "c1", "content": "ok", "is_error": false,
	})
	if !ok {
		t.Fatal("valid tool_result payload rejected")
	}
	if result.CallID != "c1" || result.Content != "ok" || result.IsError {
		t.Fatalf("result = %+v", result)
	}
	if _, ok := payloadToToolResult(map[string]any{"content": "x"}); ok {
		t.Fatal("missing tool_call_id must be rejected")
	}
}

func TestPayloadToPart(t *testing.T) {
	part, ok := payloadToPart(map[string]any{"type": "text", "text": "hi"})
	if !ok {
		t.Fatal("valid text part rejected")
	}
	if text, ok := part.(message.TextPart); !ok || text.Text != "hi" {
		t.Fatalf("part = %#v", part)
	}

	part, ok = payloadToPart(`{"type":"image","source":{"kind":"url","url":"https://x/i.png","media_type":"image/png"}}`)
	if !ok {
		t.Fatal("valid image part rejected")
	}
	if _, ok := part.(message.ImagePart); !ok {
		t.Fatalf("part = %T, want ImagePart", part)
	}

	if _, ok := payloadToPart(map[string]any{"type": "hologram"}); ok {
		t.Fatal("unknown part type must be rejected")
	}
	if _, ok := payloadToPart(nil); ok {
		t.Fatal("nil payload must be rejected")
	}
}

type emitCaptureHost struct {
	agent.NoopHost
	mu   sync.Mutex
	envs []event.Envelope
}

func (h *emitCaptureHost) Publish(_ context.Context, env event.Envelope) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.envs = append(h.envs, env)
	return nil
}

func captureEmits(t *testing.T, emit func(*agent.ScriptEnv)) []agent.StreamDeltaPayload {
	t.Helper()
	host := &emitCaptureHost{}
	rt := fakeRuntime{exec: func(_ context.Context, _, _ string, env *agent.ScriptEnv) (*agent.ScriptSignal, error) {
		emit(env)
		return nil, nil
	}}
	reg := scriptRegistry(t, ScriptNodeDeps{Runtimes: map[string]agent.ScriptRuntime{"fake": rt}})
	g := singleScriptGraph(t, reg, ScriptConfig{Runtime: "fake", Source: "x"})
	if err := executeGraphWithHost(g, host, agent.NewBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	host.mu.Lock()
	defer host.mu.Unlock()
	var deltas []agent.StreamDeltaPayload
	for _, env := range host.envs {
		if !agent.IsStreamDelta(env.Subject) {
			continue
		}
		delta, err := agent.DecodeStreamDelta(env)
		if err != nil {
			t.Fatalf("DecodeStreamDelta: %v", err)
		}
		deltas = append(deltas, delta)
	}
	return deltas
}

func TestScriptEmitter_PassthroughCarriesPayload(t *testing.T) {
	deltas := captureEmits(t, func(env *agent.ScriptEnv) {
		emit := env.Bindings["host"].(map[string]any)["emit"].(func(string, any))
		emit("progress", map[string]any{"pct": 42, "label": "x"})
		emit("custom", nil)
	})
	if len(deltas) != 2 {
		t.Fatalf("emitted %d deltas, want 2", len(deltas))
	}
	if deltas[0].Type != "progress" {
		t.Fatalf("first delta type = %q, want progress", deltas[0].Type)
	}
	var payload map[string]any
	if err := json.Unmarshal(deltas[0].Payload, &payload); err != nil {
		t.Fatalf("decode passthrough payload %s: %v", deltas[0].Payload, err)
	}
	if payload["pct"] != float64(42) || payload["label"] != "x" {
		t.Fatalf("passthrough payload = %v, want {pct:42 label:x}", payload)
	}
	if deltas[1].Type != "custom" {
		t.Fatalf("second delta type = %q, want custom", deltas[1].Type)
	}
	if deltas[1].Payload != nil {
		t.Fatalf("nil payload should not attach a payload field, got %s", deltas[1].Payload)
	}
}

func TestScriptEmitter_InvalidPayloadSkipsEmission(t *testing.T) {
	deltas := captureEmits(t, func(env *agent.ScriptEnv) {
		emit := env.Bindings["host"].(map[string]any)["emit"].(func(string, any))
		emit("tool_call", map[string]any{"name": "search"}) // missing id
		emit("tool_result", map[string]any{"content": "x"}) // missing tool_call_id
		emit("part", map[string]any{"type": "hologram"})    // unknown part kind
		emit("token", "kept")
	})
	if len(deltas) != 1 {
		t.Fatalf("emitted %d deltas, want only the valid token delta", len(deltas))
	}
	if deltas[0].Type != agent.StreamDeltaPart {
		t.Fatalf("delta type = %q, want %q", deltas[0].Type, agent.StreamDeltaPart)
	}
	text, ok := deltas[0].Part.(message.TextPart)
	if !ok || text.Text != "kept" {
		t.Fatalf("delta part = %#v, want kept text part", deltas[0].Part)
	}
}

func TestScriptEmitter_PassthroughReachesStreamSubscription(t *testing.T) {
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
		emit := env.Bindings["host"].(map[string]any)["emit"].(func(string, any))
		emit("progress", map[string]any{"pct": 42})
		if !next() {
			t.Error("next() = false, want the emitted delta")
			return nil, nil
		}
		cur := current()
		if cur["event"] != "stream.delta" || cur["type"] != "progress" {
			t.Errorf("current = %v, want stream.delta progress", cur)
			return nil, nil
		}
		payload, ok := cur["payload"].(map[string]any)
		if !ok || payload["pct"] != float64(42) {
			t.Errorf("payload projection = %v, want {pct:42}", cur["payload"])
		}
		return nil, nil
	}}
	reg := scriptRegistry(t, ScriptNodeDeps{Runtimes: map[string]agent.ScriptRuntime{"fake": rt}})
	g := singleScriptGraph(t, reg, ScriptConfig{Runtime: "fake", Source: "x"})
	if err := executeGraphWithHost(g, host, agent.NewBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}
