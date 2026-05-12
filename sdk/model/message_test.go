package model

import (
	"bytes"
	"encoding/json"
	"testing"
)

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

func TestMessage_CloneDeepCopiesParts(t *testing.T) {
	msg := Message{
		Role: RoleAssistant,
		Parts: []Part{
			{Type: PartText, Text: "hello"},
			{Type: PartImage, Image: &MediaRef{URL: "https://img.example.com/a.png"}},
			{Type: PartData, Data: &DataRef{Value: map[string]any{
				"k":      "v",
				"nested": map[string]any{"n": "v"},
				"slice":  []any{map[string]any{"s": "v"}},
			}}},
			{Type: PartToolCall, ToolCall: &ToolCall{ID: "tc1", Name: "search", Arguments: "{}"}},
			{Type: PartToolResult, ToolResult: &ToolResult{ToolCallID: "tc1", Content: "ok"}},
		},
	}

	cp := msg.Clone()
	msg.Parts[0].Text = "mutated"
	msg.Parts[1].Image.URL = "mutated"
	msg.Parts[2].Data.Value["k"] = "mutated"
	msg.Parts[2].Data.Value["nested"].(map[string]any)["n"] = "mutated"
	msg.Parts[2].Data.Value["slice"].([]any)[0].(map[string]any)["s"] = "mutated"
	msg.Parts[3].ToolCall.Name = "mutated"
	msg.Parts[4].ToolResult.Content = "mutated"

	if got := cp.Parts[0].Text; got != "hello" {
		t.Fatalf("text part leaked mutation: %q", got)
	}
	if got := cp.Parts[1].Image.URL; got != "https://img.example.com/a.png" {
		t.Fatalf("image ref leaked mutation: %q", got)
	}
	if got := cp.Parts[2].Data.Value["k"]; got != "v" {
		t.Fatalf("data value leaked mutation: %v", got)
	}
	if got := cp.Parts[2].Data.Value["nested"].(map[string]any)["n"]; got != "v" {
		t.Fatalf("nested data map leaked mutation: %v", got)
	}
	if got := cp.Parts[2].Data.Value["slice"].([]any)[0].(map[string]any)["s"]; got != "v" {
		t.Fatalf("nested data slice leaked mutation: %v", got)
	}
	if got := cp.Parts[3].ToolCall.Name; got != "search" {
		t.Fatalf("tool call leaked mutation: %q", got)
	}
	if got := cp.Parts[4].ToolResult.Content; got != "ok" {
		t.Fatalf("tool result leaked mutation: %q", got)
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

func TestTokenUsage_Add_Enriched(t *testing.T) {
	t.Run("sums latency and cost; preserves model label from accumulator", func(t *testing.T) {
		acc := TokenUsage{InputTokens: 10, Model: "gpt-4o", LatencyMs: 100, CostMicros: 250}
		delta := TokenUsage{OutputTokens: 5, Model: "claude", LatencyMs: 30, CostMicros: 80}
		sum := acc.Add(delta)
		if sum.Model != "gpt-4o" {
			t.Errorf("Model = %q, want gpt-4o (accumulator wins on conflict)", sum.Model)
		}
		if sum.LatencyMs != 130 || sum.CostMicros != 330 {
			t.Errorf("Latency=%d Cost=%d, want 130 / 330", sum.LatencyMs, sum.CostMicros)
		}
	})

	t.Run("empty accumulator inherits delta's model", func(t *testing.T) {
		acc := TokenUsage{}
		delta := TokenUsage{Model: "claude", CostMicros: 80}
		sum := acc.Add(delta)
		if sum.Model != "claude" || sum.CostMicros != 80 {
			t.Errorf("got %+v, want Model=claude Cost=80", sum)
		}
	})

	t.Run("cached input tokens sum across deltas", func(t *testing.T) {
		acc := TokenUsage{InputTokens: 100, CachedInputTokens: 60}
		delta := TokenUsage{InputTokens: 80, CachedInputTokens: 50}
		sum := acc.Add(delta)
		if sum.InputTokens != 180 || sum.CachedInputTokens != 110 {
			t.Errorf("Input=%d Cached=%d, want 180 / 110", sum.InputTokens, sum.CachedInputTokens)
		}
	})
}

func TestTokenUsage_CachedInputTokens_JSON(t *testing.T) {
	t.Run("zero value omits cached_input_tokens (back-compat with v<=0.3.4)", func(t *testing.T) {
		u := TokenUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}
		b, err := json.Marshal(u)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if bytes.Contains(b, []byte("cached_input_tokens")) {
			t.Errorf("zero value should omit cached_input_tokens, got %s", b)
		}
	})

	t.Run("non-zero serialises and round-trips", func(t *testing.T) {
		u := TokenUsage{InputTokens: 1000, CachedInputTokens: 800, OutputTokens: 50, TotalTokens: 1050}
		b, err := json.Marshal(u)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if !bytes.Contains(b, []byte(`"cached_input_tokens":800`)) {
			t.Errorf("expected cached_input_tokens in JSON, got %s", b)
		}
		var got TokenUsage
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if got.CachedInputTokens != 800 {
			t.Errorf("round-trip CachedInputTokens = %d, want 800", got.CachedInputTokens)
		}
	})
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

func TestLastByRole(t *testing.T) {
	msgs := []Message{
		NewTextMessage(RoleUser, "u1"),
		NewTextMessage(RoleAssistant, "a1"),
		NewTextMessage(RoleUser, "u2"),
		NewTextMessage(RoleAssistant, "a2"),
	}

	if m, ok := LastByRole(msgs, RoleUser); !ok || m.Content() != "u2" {
		t.Fatalf("LastByRole(user) = (%q, %v), want (\"u2\", true)", m.Content(), ok)
	}
	if m, ok := LastByRole(msgs, RoleAssistant); !ok || m.Content() != "a2" {
		t.Fatalf("LastByRole(assistant) = (%q, %v), want (\"a2\", true)", m.Content(), ok)
	}
	if _, ok := LastByRole(msgs, RoleSystem); ok {
		t.Fatal("LastByRole(system) should report not-found on a transcript without system turns")
	}
	if _, ok := LastByRole(nil, RoleUser); ok {
		t.Fatal("LastByRole on nil slice should report not-found")
	}
	if _, ok := LastByRole([]Message{}, RoleUser); ok {
		t.Fatal("LastByRole on empty slice should report not-found")
	}
}
