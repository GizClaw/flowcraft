package history

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestNewBuffer_Defaults(t *testing.T) {
	mem := NewBuffer(NewInMemoryStore())
	if mem == nil {
		t.Fatal("NewBuffer returned nil")
	}
	ctx := context.Background()
	if err := mem.Append(ctx, "c1", []model.Message{model.NewTextMessage(model.RoleUser, "hello")}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	msgs, err := mem.Load(ctx, "c1", Budget{})
	if err != nil || len(msgs) != 1 {
		t.Fatalf("Load: %v len=%d", err, len(msgs))
	}
}

func TestNewBuffer_WithMaxApplies(t *testing.T) {
	mem := NewBuffer(NewInMemoryStore(), WithBufferMax(2))
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := mem.Append(ctx, "c", []model.Message{model.NewTextMessage(model.RoleUser, "m")}); err != nil {
			t.Fatal(err)
		}
	}
	msgs, _ := mem.Load(ctx, "c", Budget{})
	if len(msgs) != 2 {
		t.Fatalf("expected tail truncation to 2, got %d", len(msgs))
	}
}

func TestNewCompacted_SmokeBoots(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mem := NewCompacted(NewInMemoryStore(), &mockSummaryLLM{}, ws, WithStoragePrefix("hst"))
	if mem == nil {
		t.Fatal("NewCompacted returned nil")
	}
	ctx := context.Background()
	if err := mem.Append(ctx, "c", []model.Message{model.NewTextMessage(model.RoleUser, "hi")}); err != nil {
		t.Fatalf("Append: %v", err)
	}
}
