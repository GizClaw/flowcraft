package llm

import (
	"context"
	"testing"
)

func TestWithLimits_Zero_ReturnsInner(t *testing.T) {
	inner := &defaultsCaptureLLM{}
	if got := WithLimits(inner, ModelLimits{}); got != inner {
		t.Fatal("zero limits should return inner unwrapped")
	}
}

func TestWithLimits_ClampsMaxTokens(t *testing.T) {
	inner := &defaultsCaptureLLM{}
	wrapped := WithLimits(inner, ModelLimits{MaxOutputTokens: 100})
	_, _, _ = wrapped.Generate(context.Background(), nil, WithMaxTokens(500))
	if *inner.captured.MaxTokens != 100 {
		t.Fatalf("MaxTokens not clamped: got %d, want 100", *inner.captured.MaxTokens)
	}
}

func TestWithLimits_LeavesUndersizedAlone(t *testing.T) {
	inner := &defaultsCaptureLLM{}
	wrapped := WithLimits(inner, ModelLimits{MaxOutputTokens: 1000})
	_, _, _ = wrapped.Generate(context.Background(), nil, WithMaxTokens(50))
	if *inner.captured.MaxTokens != 50 {
		t.Fatalf("under-limit MaxTokens should pass through: got %d", *inner.captured.MaxTokens)
	}
}

func TestWithLimits_NilMaxTokens_NotInjected(t *testing.T) {
	// Limits clamps; it must NOT *set* MaxTokens when caller left it nil.
	inner := &defaultsCaptureLLM{}
	wrapped := WithLimits(inner, ModelLimits{MaxOutputTokens: 100})
	_, _, _ = wrapped.Generate(context.Background(), nil)
	if inner.captured.MaxTokens != nil {
		t.Fatalf("limits should never set MaxTokens; got %v", *inner.captured.MaxTokens)
	}
}

func TestWithLimits_TruncatesTools(t *testing.T) {
	inner := &defaultsCaptureLLM{}
	wrapped := WithLimits(inner, ModelLimits{MaxToolDefinitions: 2})
	tools := []ToolDefinition{
		{Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"},
	}
	_, _, _ = wrapped.Generate(context.Background(), nil, WithTools(tools...))
	if len(inner.captured.Tools) != 2 {
		t.Fatalf("Tools not truncated: got %d, want 2", len(inner.captured.Tools))
	}
	if inner.captured.Tools[0].Name != "a" || inner.captured.Tools[1].Name != "b" {
		t.Fatalf("expected head-truncation [a,b]; got %v", inner.captured.Tools)
	}
}
