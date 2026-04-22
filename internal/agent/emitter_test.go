package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	"github.com/GizClaw/flowcraft/internal/eventlogtest"
)

func TestEmitter_RunLifecycle(t *testing.T) {
	log := eventlogtest.NewMemoryLog()
	em := NewEmitter(log)
	ctx := context.Background()

	if err := em.RunStarted(ctx, "card-1", "run-1", eventlog.Actor{Kind: "user", ID: "u1"}); err != nil {
		t.Fatalf("RunStarted: %v", err)
	}
	if err := em.RunCompleted(ctx, "card-1", "run-1", "done", eventlog.Actor{Kind: "user", ID: "u1"}); err != nil {
		t.Fatalf("RunCompleted: %v", err)
	}

	res, err := log.ReadAll(ctx, eventlog.SinceBeginning, 10)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(res.Events) != 2 {
		t.Fatalf("want 2 envelopes, got %d", len(res.Events))
	}
	if res.Events[0].Type != eventlog.EventTypeAgentRunStarted {
		t.Fatalf("first envelope type %q", res.Events[0].Type)
	}
	if res.Events[1].Type != eventlog.EventTypeAgentRunCompleted {
		t.Fatalf("second envelope type %q", res.Events[1].Type)
	}
}

func TestDeltaFlusher_BatchesAndFinishes(t *testing.T) {
	log := eventlogtest.NewMemoryLog()
	em := NewEmitter(log)
	ctx := context.Background()

	flusher := NewDeltaFlusher(em, "card-1", "run-1", "", "conv-1", eventlog.Actor{Kind: "agent", ID: "agent-7"}, FlusherOptions{
		MaxBatchBytes:    16,
		MaxBatchInterval: 50 * time.Millisecond,
	})

	flusher.Push(ctx, "hello, world. ")
	flusher.Push(ctx, "more text incoming")
	time.Sleep(100 * time.Millisecond)
	flusher.Close(ctx)

	res, err := log.ReadAll(ctx, eventlog.SinceBeginning, 20)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(res.Events) < 1 {
		t.Fatalf("expected at least 1 delta envelope, got 0")
	}
	last := res.Events[len(res.Events)-1]
	var p eventlog.AgentStreamDeltaPayload
	if err := json.Unmarshal(last.Payload, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if !p.Finished {
		t.Fatalf("last delta should be finished=true, got payload=%+v", p)
	}
	if p.ConversationID != "conv-1" {
		t.Fatalf("conversation_id mismatch: %q", p.ConversationID)
	}

	// delta_seq must be strictly monotonic
	prev := int64(0)
	for _, env := range res.Events {
		var pp eventlog.AgentStreamDeltaPayload
		if err := json.Unmarshal(env.Payload, &pp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if pp.DeltaSeq <= prev {
			t.Fatalf("delta_seq must be strictly monotonic, got %d after %d", pp.DeltaSeq, prev)
		}
		prev = pp.DeltaSeq
	}
}

func TestDeltaFlusher_ThinkingRole(t *testing.T) {
	log := eventlogtest.NewMemoryLog()
	em := NewEmitter(log)
	ctx := context.Background()

	flusher := NewDeltaFlusher(em, "card-1", "run-1", "thinking", "conv-1", eventlog.Actor{Kind: "agent", ID: "agent-7"}, FlusherOptions{})
	flusher.Push(ctx, "internal monologue")
	flusher.Close(ctx)

	res, _ := log.ReadAll(ctx, eventlog.SinceBeginning, 20)
	if len(res.Events) == 0 {
		t.Fatal("expected thinking delta envelope")
	}
	for _, env := range res.Events {
		if env.Type != eventlog.EventTypeAgentThinkingDelta {
			t.Fatalf("expected agent.thinking.delta, got %s", env.Type)
		}
	}
}
