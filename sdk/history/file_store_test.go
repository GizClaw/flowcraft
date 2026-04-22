package history

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestFileStore_SaveAndLoad(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFileStore(ws, "memory")
	ctx := context.Background()

	msgs := []model.Message{
		model.NewTextMessage(model.RoleUser, "hello"),
		model.NewTextMessage(model.RoleAssistant, "hi there"),
	}
	if err := store.SaveMessages(ctx, "conv1", msgs); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.GetMessages(ctx, "conv1")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 {
		t.Fatalf("expected 2, got %d", len(loaded))
	}
	if loaded[0].Content() != "hello" {
		t.Fatalf("expected hello, got %q", loaded[0].Content())
	}
	if loaded[1].Content() != "hi there" {
		t.Fatalf("expected hi there, got %q", loaded[1].Content())
	}
}

func TestFileStore_IncrementalAppend(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFileStore(ws, "memory")
	ctx := context.Background()

	round1 := []model.Message{
		model.NewTextMessage(model.RoleUser, "msg1"),
		model.NewTextMessage(model.RoleAssistant, "reply1"),
	}
	if err := store.SaveMessages(ctx, "conv1", round1); err != nil {
		t.Fatal(err)
	}

	// Second save with appended messages
	round2 := append(round1,
		model.NewTextMessage(model.RoleUser, "msg2"),
		model.NewTextMessage(model.RoleAssistant, "reply2"),
	)
	if err := store.SaveMessages(ctx, "conv1", round2); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.GetMessages(ctx, "conv1")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 4 {
		t.Fatalf("expected 4, got %d", len(loaded))
	}
	if loaded[2].Content() != "msg2" {
		t.Fatalf("expected msg2, got %q", loaded[2].Content())
	}
}

func TestFileStore_PersistsAcrossInstances(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	ctx := context.Background()

	// First "process": save messages
	store1 := NewFileStore(ws, "memory")
	msgs := []model.Message{
		model.NewTextMessage(model.RoleUser, "persistent msg"),
		model.NewTextMessage(model.RoleAssistant, "persistent reply"),
	}
	if err := store1.SaveMessages(ctx, "conv1", msgs); err != nil {
		t.Fatal(err)
	}

	// Second "process": new FileStore instance, same workspace
	store2 := NewFileStore(ws, "memory")
	loaded, err := store2.GetMessages(ctx, "conv1")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 {
		t.Fatalf("expected 2 messages after restart, got %d", len(loaded))
	}
	if loaded[0].Content() != "persistent msg" {
		t.Fatalf("expected persistent msg, got %q", loaded[0].Content())
	}
}

func TestFileStore_DeleteMessages(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFileStore(ws, "memory")
	ctx := context.Background()

	msgs := []model.Message{model.NewTextMessage(model.RoleUser, "to delete")}
	_ = store.SaveMessages(ctx, "conv1", msgs)

	if err := store.DeleteMessages(ctx, "conv1"); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.GetMessages(ctx, "conv1")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 0 {
		t.Fatalf("expected 0 after delete, got %d", len(loaded))
	}
}

func TestFileStore_EmptyConversation(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFileStore(ws, "memory")
	ctx := context.Background()

	loaded, err := store.GetMessages(ctx, "nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 0 {
		t.Fatalf("expected 0 for nonexistent, got %d", len(loaded))
	}
}

func TestFileStore_Isolation(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFileStore(ws, "memory")
	ctx := context.Background()

	_ = store.SaveMessages(ctx, "a", []model.Message{model.NewTextMessage(model.RoleUser, "msg-a")})
	_ = store.SaveMessages(ctx, "b", []model.Message{model.NewTextMessage(model.RoleUser, "msg-b")})

	msgsA, _ := store.GetMessages(ctx, "a")
	msgsB, _ := store.GetMessages(ctx, "b")

	if len(msgsA) != 1 || msgsA[0].Content() != "msg-a" {
		t.Fatal("isolation broken for conv a")
	}
	if len(msgsB) != 1 || msgsB[0].Content() != "msg-b" {
		t.Fatal("isolation broken for conv b")
	}
}

func TestBufferMemory_WithFileStore(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFileStore(ws, "memory")
	ctx := context.Background()
	convID := "copilot-session"

	round1 := []model.Message{
		model.NewTextMessage(model.RoleUser, "turn1"),
		model.NewTextMessage(model.RoleAssistant, "reply1"),
	}
	buf := NewBufferMemory(store, 50)
	if err := buf.Append(ctx, convID, round1); err != nil {
		t.Fatal(err)
	}

	loaded, err := buf.Load(ctx, convID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 {
		t.Fatalf("expected 2 from round 1, got %d", len(loaded))
	}

	round2 := []model.Message{
		model.NewTextMessage(model.RoleUser, "turn2"),
		model.NewTextMessage(model.RoleAssistant, "reply2"),
	}
	if err := buf.Append(ctx, convID, round2); err != nil {
		t.Fatal(err)
	}

	loaded, _ = buf.Load(ctx, convID)
	if len(loaded) != 4 {
		t.Fatalf("expected 4 after 2 rounds, got %d", len(loaded))
	}
}

func TestBufferMemory_FileStoreSurvivesRestart(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	ctx := context.Background()
	convID := "restart-test"

	store1 := NewFileStore(ws, "memory")
	buf1 := NewBufferMemory(store1, 50)
	msgs := []model.Message{
		model.NewTextMessage(model.RoleUser, "before restart"),
		model.NewTextMessage(model.RoleAssistant, "still here"),
	}
	if err := buf1.Append(ctx, convID, msgs); err != nil {
		t.Fatal(err)
	}

	store2 := NewFileStore(ws, "memory")
	buf2 := NewBufferMemory(store2, 50)
	loaded, err := buf2.Load(ctx, convID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 {
		t.Fatalf("expected 2 messages after restart, got %d", len(loaded))
	}
	if loaded[0].Content() != "before restart" {
		t.Fatalf("expected 'before restart', got %q", loaded[0].Content())
	}
	if loaded[1].Content() != "still here" {
		t.Fatalf("expected 'still here', got %q", loaded[1].Content())
	}
}

func TestFileStore_ToolCallMessages(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFileStore(ws, "memory")
	ctx := context.Background()

	msgs := []model.Message{
		model.NewTextMessage(model.RoleUser, "search for cats"),
		{
			Role: model.RoleAssistant,
			Parts: []model.Part{
				{Type: model.PartText, Text: "Let me search for that."},
				{Type: model.PartToolCall, ToolCall: &model.ToolCall{ID: "tc1", Name: "search", Arguments: `{"q":"cats"}`}},
			},
		},
		{
			Role: model.RoleTool,
			Parts: []model.Part{
				{Type: model.PartToolResult, ToolResult: &model.ToolResult{ToolCallID: "tc1", Content: "Found 5 results"}},
			},
		},
	}
	if err := store.SaveMessages(ctx, "conv-tools", msgs); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.GetMessages(ctx, "conv-tools")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 3 {
		t.Fatalf("expected 3, got %d", len(loaded))
	}
	calls := loaded[1].ToolCalls()
	if len(calls) != 1 || calls[0].Name != "search" {
		t.Fatal("tool call not preserved after save/load")
	}
	results := loaded[2].ToolResults()
	if len(results) != 1 || results[0].ToolCallID != "tc1" {
		t.Fatal("tool result not preserved after save/load")
	}
}

func TestFileStore_LargeMessages(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFileStore(ws, "memory")
	ctx := context.Background()

	// Create a message with >64KB content to exercise the increased scanner buffer
	bigArgs := strings.Repeat("x", 100*1024) // 100KB
	msgs := []model.Message{
		{
			Role: model.RoleAssistant,
			Parts: []model.Part{
				{Type: model.PartToolCall, ToolCall: &model.ToolCall{ID: "tc1", Name: "big_tool", Arguments: bigArgs}},
			},
		},
	}
	if err := store.SaveMessages(ctx, "conv-big", msgs); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.GetMessages(ctx, "conv-big")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1, got %d", len(loaded))
	}
	calls := loaded[0].ToolCalls()
	if len(calls) != 1 || len(calls[0].Arguments) != 100*1024 {
		t.Fatal("large tool call arguments not preserved")
	}
}

func TestFileStore_DeleteMessages_CleansLock(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFileStore(ws, "memory")
	ctx := context.Background()

	msgs := []model.Message{model.NewTextMessage(model.RoleUser, "test")}
	_ = store.SaveMessages(ctx, "conv-lock", msgs)

	// Verify lock exists
	store.mu.Lock()
	_, lockExists := store.locks["conv-lock"]
	store.mu.Unlock()
	if !lockExists {
		t.Fatal("expected lock to exist after SaveMessages")
	}

	if err := store.DeleteMessages(ctx, "conv-lock"); err != nil {
		t.Fatal(err)
	}

	// Verify lock is cleaned up
	store.mu.Lock()
	_, lockExists = store.locks["conv-lock"]
	store.mu.Unlock()
	if lockExists {
		t.Fatal("expected lock to be removed after DeleteMessages")
	}
}

func TestFileStore_DeleteMessages_ConcurrentAccess(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFileStore(ws, "memory")
	ctx := context.Background()

	msgs := []model.Message{model.NewTextMessage(model.RoleUser, "test")}
	_ = store.SaveMessages(ctx, "conv-concurrent", msgs)

	// Delete and immediately reuse from multiple goroutines
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = store.DeleteMessages(ctx, "conv-concurrent")
	}()
	<-done

	// After delete, a new save should work with a fresh lock
	newMsgs := []model.Message{model.NewTextMessage(model.RoleUser, "new")}
	if err := store.SaveMessages(ctx, "conv-concurrent", newMsgs); err != nil {
		t.Fatalf("save after delete should work: %v", err)
	}

	loaded, err := store.GetMessages(ctx, "conv-concurrent")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].Content() != "new" {
		t.Fatalf("expected 'new', got %v", loaded)
	}
}

func TestFileStore_DeleteAndReuse(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFileStore(ws, "memory")
	ctx := context.Background()

	msgs := []model.Message{model.NewTextMessage(model.RoleUser, "original")}
	_ = store.SaveMessages(ctx, "reuse", msgs)
	if err := store.DeleteMessages(ctx, "reuse"); err != nil {
		t.Fatal(err)
	}

	// Reuse same conversation ID
	newMsgs := []model.Message{model.NewTextMessage(model.RoleUser, "new message")}
	if err := store.SaveMessages(ctx, "reuse", newMsgs); err != nil {
		t.Fatalf("should be able to reuse conversation after delete: %v", err)
	}
	loaded, err := store.GetMessages(ctx, "reuse")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].Content() != "new message" {
		t.Fatalf("expected 'new message', got %v", loaded)
	}
}
