package history

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/model"
)

func TestMemoryExpandTool_NoConversationID(t *testing.T) {
	tool := newHistoryExpandTool(ToolDeps{})
	_, err := tool.Execute(context.Background(), `{"summary_id":"n1"}`)
	if err == nil || !strings.Contains(err.Error(), "no conversation ID") {
		t.Fatalf("expected no conversation ID error, got: %v", err)
	}
}

func TestMemoryCompactTool_Definition(t *testing.T) {
	tool := newHistoryCompactTool(ToolDeps{})
	def := tool.Definition()
	if def.Name != "history_compact" {
		t.Fatalf("expected name history_compact, got %s", def.Name)
	}
}

func TestConversationIDContext(t *testing.T) {
	ctx := context.Background()
	if id := ConversationIDFrom(ctx); id != "" {
		t.Fatal("expected empty")
	}

	ctx = WithConversationID(ctx, "test-123")
	if id := ConversationIDFrom(ctx); id != "test-123" {
		t.Fatalf("expected test-123, got %q", id)
	}
}

func TestFileStore_GetMessageRange(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()
	convID := "range-test"

	msgs := make([]model.Message, 20)
	for i := range msgs {
		msgs[i] = model.NewTextMessage(model.RoleUser, "msg")
	}
	_ = store.SaveMessages(ctx, convID, msgs)

	// InMemoryStore doesn't implement RangeReader, so test FileStore.
	// Just verify the interface is defined.
	var _ RangeReader = (*FileStore)(nil)
}
