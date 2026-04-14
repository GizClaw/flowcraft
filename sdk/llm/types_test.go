package llm

import "testing"

func TestMessage_Content(t *testing.T) {
	msg := Message{
		Role: RoleAssistant,
		Parts: []Part{
			{Type: PartText, Text: "hello "},
			{Type: PartText, Text: "world"},
			{Type: PartToolCall, ToolCall: &ToolCall{ID: "1", Name: "test", Arguments: "{}"}},
		},
	}
	if got := msg.Content(); got != "hello world" {
		t.Fatalf("Content() = %q, want %q", got, "hello world")
	}
}

func TestMessage_ToolCalls(t *testing.T) {
	tc := ToolCall{ID: "tc1", Name: "search", Arguments: `{"q":"test"}`}
	msg := NewToolCallMessage([]ToolCall{tc})

	calls := msg.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("ToolCalls() len = %d, want 1", len(calls))
	}
	if calls[0].Name != "search" {
		t.Fatalf("ToolCalls()[0].Name = %q, want %q", calls[0].Name, "search")
	}
	if !msg.HasToolCalls() {
		t.Fatal("HasToolCalls() = false, want true")
	}
}

func TestMessage_ToolResults(t *testing.T) {
	tr := ToolResult{ToolCallID: "tc1", Content: "found", IsError: false}
	msg := NewToolResultMessage([]ToolResult{tr})

	results := msg.ToolResults()
	if len(results) != 1 {
		t.Fatalf("ToolResults() len = %d, want 1", len(results))
	}
	if results[0].Content != "found" {
		t.Fatalf("ToolResults()[0].Content = %q, want %q", results[0].Content, "found")
	}
}

func TestNewTextMessage(t *testing.T) {
	msg := NewTextMessage(RoleUser, "hi")
	if msg.Role != RoleUser {
		t.Fatalf("Role = %q, want %q", msg.Role, RoleUser)
	}
	if msg.Content() != "hi" {
		t.Fatalf("Content() = %q, want %q", msg.Content(), "hi")
	}
}

func TestNewImageMessage(t *testing.T) {
	msg := NewImageMessage(RoleUser, "describe this", "https://img.example.com/cat.jpg")
	if len(msg.Parts) != 2 {
		t.Fatalf("Parts len = %d, want 2", len(msg.Parts))
	}
	if msg.Parts[1].Type != PartImage {
		t.Fatalf("Parts[1].Type = %q, want %q", msg.Parts[1].Type, PartImage)
	}
	if msg.Parts[1].Image.URL != "https://img.example.com/cat.jpg" {
		t.Fatalf("Parts[1].Image.URL = %q", msg.Parts[1].Image.URL)
	}
}

func TestTokenUsage_Add(t *testing.T) {
	a := TokenUsage{InputTokens: 10, OutputTokens: 20, TotalTokens: 30}
	b := TokenUsage{InputTokens: 5, OutputTokens: 15, TotalTokens: 20}
	sum := a.Add(b)

	if sum.InputTokens != 15 || sum.OutputTokens != 35 || sum.TotalTokens != 50 {
		t.Fatalf("Add() = %+v, want {15 35 50}", sum)
	}
}

func TestMessage_NoToolCalls(t *testing.T) {
	msg := NewTextMessage(RoleAssistant, "just text")
	if msg.HasToolCalls() {
		t.Fatal("HasToolCalls() = true for text-only message")
	}
	if len(msg.ToolCalls()) != 0 {
		t.Fatal("ToolCalls() should be empty")
	}
}

func TestMarshalToolArgs(t *testing.T) {
	result, err := MarshalToolArgs(map[string]string{"key": "value"})
	if err != nil {
		t.Fatalf("MarshalToolArgs error: %v", err)
	}
	if result != `{"key":"value"}` {
		t.Fatalf("MarshalToolArgs = %q", result)
	}
}

func TestMarshalToolArgs_Error(t *testing.T) {
	_, err := MarshalToolArgs(make(chan int))
	if err == nil {
		t.Fatal("MarshalToolArgs should return error for unsupported type")
	}
}
