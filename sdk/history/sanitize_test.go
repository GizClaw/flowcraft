package history

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/model"
)

func TestSanitizeToolPairs_OrphanedToolResult(t *testing.T) {
	msgs := []model.Message{
		model.NewTextMessage(model.RoleSystem, "sys"),
		model.NewTextMessage(model.RoleUser, "hello"),
		// tool_result references a tool_use that was truncated away
		model.NewToolResultMessage([]model.ToolResult{
			{ToolCallID: "missing_id", Content: "result"},
		}),
		model.NewTextMessage(model.RoleUser, "continue"),
		model.NewTextMessage(model.RoleAssistant, "ok"),
	}

	got := sanitizeToolPairs(msgs)

	for _, m := range got {
		if m.Role == model.RoleTool {
			t.Fatal("orphaned tool_result message should have been removed")
		}
	}
	if len(got) != 4 {
		t.Fatalf("expected 4 messages (sys+user+user+assistant), got %d", len(got))
	}
}

func TestSanitizeToolPairs_TrailingToolUsePreserved(t *testing.T) {
	// tool_use at the end without a result is legitimate (tool still pending).
	msgs := []model.Message{
		model.NewTextMessage(model.RoleUser, "do something"),
		model.NewToolCallMessage([]model.ToolCall{
			{ID: "tc_1", Name: "search", Arguments: "{}"},
		}),
	}

	got := sanitizeToolPairs(msgs)

	if len(got) != 2 {
		t.Fatalf("expected 2 messages preserved, got %d", len(got))
	}
	if !got[1].HasToolCalls() {
		t.Fatal("trailing tool_use should be preserved")
	}
}

func TestSanitizeToolPairs_MatchedPairPreserved(t *testing.T) {
	msgs := []model.Message{
		model.NewTextMessage(model.RoleSystem, "sys"),
		model.NewTextMessage(model.RoleUser, "query"),
		model.NewToolCallMessage([]model.ToolCall{
			{ID: "tc_1", Name: "search", Arguments: `{"q":"test"}`},
		}),
		model.NewToolResultMessage([]model.ToolResult{
			{ToolCallID: "tc_1", Content: "found it"},
		}),
		model.NewTextMessage(model.RoleAssistant, "here is the answer"),
	}

	got := sanitizeToolPairs(msgs)

	if len(got) != 5 {
		t.Fatalf("expected all 5 messages preserved, got %d", len(got))
	}
	if !got[2].HasToolCalls() {
		t.Fatal("tool_call message should be preserved")
	}
	if len(got[3].ToolResults()) != 1 {
		t.Fatal("tool_result message should be preserved")
	}
}

func TestSanitizeToolPairs_MixedPartsPreserved(t *testing.T) {
	// Assistant message with text + tool_call but no result yet — both preserved.
	assistantMsg := model.Message{
		Role: model.RoleAssistant,
		Parts: []model.Part{
			{Type: model.PartText, Text: "Let me search for that."},
			{Type: model.PartToolCall, ToolCall: &model.ToolCall{ID: "tc_pending", Name: "search", Arguments: "{}"}},
		},
	}
	msgs := []model.Message{
		model.NewTextMessage(model.RoleUser, "find something"),
		assistantMsg,
	}

	got := sanitizeToolPairs(msgs)

	if len(got) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(got))
	}
	if !got[1].HasToolCalls() {
		t.Fatal("pending tool_call should be preserved")
	}
	if got[1].Content() != "Let me search for that." {
		t.Fatalf("text part should be preserved, got %q", got[1].Content())
	}
}

func TestSanitizeToolPairs_AllToolCallsPreserved(t *testing.T) {
	// Assistant has two tool_calls, only one has a result — both tool_calls
	// stay because the other might still be pending.
	msgs := []model.Message{
		model.NewTextMessage(model.RoleUser, "do two things"),
		{
			Role: model.RoleAssistant,
			Parts: []model.Part{
				{Type: model.PartToolCall, ToolCall: &model.ToolCall{ID: "tc_a", Name: "tool_a", Arguments: "{}"}},
				{Type: model.PartToolCall, ToolCall: &model.ToolCall{ID: "tc_b", Name: "tool_b", Arguments: "{}"}},
			},
		},
		model.NewToolResultMessage([]model.ToolResult{
			{ToolCallID: "tc_a", Content: "result_a"},
		}),
		model.NewTextMessage(model.RoleAssistant, "done"),
	}

	got := sanitizeToolPairs(msgs)

	if len(got) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(got))
	}
	calls := got[1].ToolCalls()
	if len(calls) != 2 {
		t.Fatalf("expected both tool_calls preserved, got %d", len(calls))
	}
}

func TestSanitizeToolPairs_NoToolMessages(t *testing.T) {
	msgs := []model.Message{
		model.NewTextMessage(model.RoleSystem, "sys"),
		model.NewTextMessage(model.RoleUser, "hello"),
		model.NewTextMessage(model.RoleAssistant, "hi"),
	}

	got := sanitizeToolPairs(msgs)

	if len(got) != 3 {
		t.Fatalf("expected 3 messages unchanged, got %d", len(got))
	}
}
