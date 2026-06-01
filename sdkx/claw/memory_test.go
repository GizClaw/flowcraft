package claw

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/sdk/model"
)

func TestSaveTurnDrainsSideEffects(t *testing.T) {
	mem, err := recall.New()
	if err != nil {
		t.Fatalf("recall.New: %v", err)
	}
	defer mem.Close()

	side := &recordingSideEffectProcessor{}
	runtime := &memoryRuntime{
		mem:  mem,
		side: side,
		scope: recall.Scope{
			RuntimeID: "rt",
			UserID:    "u",
			AgentID:   "a",
		},
		cfg: MemoryConfig{
			Write: MemoryWriteConfig{SaveConversation: true},
		},
	}
	if err := runtime.saveTurn(context.Background(), "ctx", "hello", model.NewTextMessage(model.RoleAssistant, "hi")); err != nil {
		t.Fatalf("saveTurn: %v", err)
	}
	if side.calls != 1 {
		t.Fatalf("side-effect drain calls = %d, want 1", side.calls)
	}
	if side.last.WorkerID != "claw" {
		t.Fatalf("worker id = %q, want claw", side.last.WorkerID)
	}
	if side.last.Scope.PartitionKey() != runtime.scope.PartitionKey() {
		t.Fatalf("scope = %+v, want %+v", side.last.Scope, runtime.scope)
	}
	if side.last.Limit <= 0 {
		t.Fatalf("limit = %d, want positive", side.last.Limit)
	}
}

type recordingSideEffectProcessor struct {
	calls int
	last  recall.SideEffectProcessOptions
}

func (p *recordingSideEffectProcessor) ProcessSideEffects(_ context.Context, opts recall.SideEffectProcessOptions) (recall.SideEffectProcessResult, error) {
	p.calls++
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	p.last = opts
	return recall.SideEffectProcessResult{Claimed: 1, Completed: 1}, nil
}
