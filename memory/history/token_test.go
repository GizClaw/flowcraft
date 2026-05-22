package history

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/model"
)

func TestTiktokenCounter_KnownModel(t *testing.T) {
	c, err := NewTiktokenCounter("gpt-4o")
	if err != nil {
		t.Fatalf("NewTiktokenCounter: %v", err)
	}
	if got := c.Count("hello world"); got <= 0 {
		t.Fatalf("expected positive token count, got %d", got)
	}
}

func TestTiktokenCounter_UnknownModelFallsBack(t *testing.T) {
	// An unknown model name should fall back to cl100k_base rather than
	// fail outright; this is the documented contract.
	c, err := NewTiktokenCounter("definitely-not-a-real-model-xyz")
	if err != nil {
		t.Fatalf("expected fallback to cl100k_base, got error: %v", err)
	}
	if c == nil || c.tk == nil {
		t.Fatal("expected non-nil counter after fallback")
	}
	if c.Count("hello") <= 0 {
		t.Fatal("fallback counter should still tokenize")
	}
}

func TestTiktokenCounterFromEncoding_Valid(t *testing.T) {
	c, err := NewTiktokenCounterFromEncoding("cl100k_base")
	if err != nil {
		t.Fatalf("NewTiktokenCounterFromEncoding: %v", err)
	}
	if c.Count("the quick brown fox") <= 0 {
		t.Fatal("expected positive token count")
	}
}

func TestTiktokenCounterFromEncoding_Invalid(t *testing.T) {
	_, err := NewTiktokenCounterFromEncoding("not_a_real_encoding")
	if err == nil {
		t.Fatal("expected error for unknown encoding")
	}
}

func TestTiktokenCounter_CountMessages_AllPartTypes(t *testing.T) {
	c, err := NewTiktokenCounter("gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	msgs := []model.Message{
		model.NewTextMessage(model.RoleUser, "hello"),
		{
			Role: model.RoleAssistant,
			Parts: []model.Part{
				{Type: model.PartText, Text: "ok"},
				{Type: model.PartToolCall, ToolCall: &model.ToolCall{ID: "1", Name: "search", Arguments: `{"q":"x"}`}},
			},
		},
		{
			Role: model.RoleTool,
			Parts: []model.Part{
				{Type: model.PartToolResult, ToolResult: &model.ToolResult{ToolCallID: "1", Content: "found"}},
			},
		},
	}
	got := c.CountMessages(msgs)
	if got <= 0 {
		t.Fatalf("expected positive token count across all part types, got %d", got)
	}
	// The empty input should still account for the "+3" priming overhead.
	if c.CountMessages(nil) != 3 {
		t.Fatalf("expected priming overhead of 3 for empty input, got %d", c.CountMessages(nil))
	}
}
