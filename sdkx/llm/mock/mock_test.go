package mock

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

func TestMockE2EGenerate_ReturnsKanbanSubmitToolCall(t *testing.T) {
	m := &MockLLM{model: "mock-e2e"}
	msg, _, err := m.Generate(context.Background(), []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "[E2E_DISPATCH target=worker-1] build a callback demo"),
	}, llm.WithTools(llm.ToolDefinition{Name: "kanban_submit"}))
	if err != nil {
		t.Fatal(err)
	}
	if !msg.HasToolCalls() {
		t.Fatal("expected tool call response")
	}
	call := msg.ToolCalls()[0]
	if call.Name != "kanban_submit" {
		t.Fatalf("tool name = %q, want kanban_submit", call.Name)
	}
	if call.Arguments == "" {
		t.Fatal("expected non-empty tool arguments")
	}
}

func TestMockE2EGenerate_ReturnsCallbackText(t *testing.T) {
	m := &MockLLM{model: "mock-e2e"}
	msg, _, err := m.Generate(context.Background(), []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "[Task Callback] card_id=card-1\n\nSummary: done"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := msg.Content(); got != "E2E callback processed successfully." {
		t.Fatalf("content = %q", got)
	}
}
