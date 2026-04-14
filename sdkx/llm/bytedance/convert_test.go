package bytedance

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/model"
)

func TestConvertMessages_MultipleToolResults(t *testing.T) {
	msgs := []model.Message{
		{
			Role: model.RoleTool,
			Parts: []model.Part{
				{Type: model.PartToolResult, ToolResult: &model.ToolResult{ToolCallID: "tc1", Content: "result1"}},
				{Type: model.PartToolResult, ToolResult: &model.ToolResult{ToolCallID: "tc2", Content: "result2"}},
				{Type: model.PartToolResult, ToolResult: &model.ToolResult{ToolCallID: "tc3", Content: "result3"}},
			},
		},
	}

	out := convertMessages(msgs)
	if len(out) != 3 {
		t.Fatalf("expected 3 messages (one per tool result), got %d", len(out))
	}

	for i, msg := range out {
		expectedID := msgs[0].Parts[i].ToolResult.ToolCallID
		if msg.ToolCallID != expectedID {
			t.Errorf("message %d: expected ToolCallID %q, got %q", i, expectedID, msg.ToolCallID)
		}
		if msg.Content == nil || msg.Content.StringValue == nil {
			t.Fatalf("message %d: expected non-nil content", i)
		}
		expectedContent := msgs[0].Parts[i].ToolResult.Content
		if *msg.Content.StringValue != expectedContent {
			t.Errorf("message %d: expected content %q, got %q", i, expectedContent, *msg.Content.StringValue)
		}
	}
}

func TestConvertMessages_SingleToolResult(t *testing.T) {
	msgs := []model.Message{
		{
			Role: model.RoleTool,
			Parts: []model.Part{
				{Type: model.PartToolResult, ToolResult: &model.ToolResult{ToolCallID: "tc1", Content: "only result"}},
			},
		},
	}

	out := convertMessages(msgs)
	if len(out) != 1 {
		t.Fatalf("expected 1, got %d", len(out))
	}
	if out[0].ToolCallID != "tc1" {
		t.Fatalf("expected tc1, got %s", out[0].ToolCallID)
	}
}

func TestConvertMessages_EmptyToolResults(t *testing.T) {
	msgs := []model.Message{
		{
			Role:  model.RoleTool,
			Parts: []model.Part{},
		},
	}

	out := convertMessages(msgs)
	if len(out) != 0 {
		t.Fatalf("expected 0 messages for empty tool results, got %d", len(out))
	}
}
