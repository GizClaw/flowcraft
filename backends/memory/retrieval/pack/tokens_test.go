package pack

import (
	"testing"

	coremessage "github.com/GizClaw/flowcraft/core/message"
)

func TestEstimatedTokensKeepsASCIIBudget(t *testing.T) {
	for _, test := range []struct {
		text string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"abcd", 1},
		{"abcde", 2},
		{"hello world", 3}, // 11 runes, ceil(11/4)
	} {
		if got := EstimatedTokens(test.text); got != test.want {
			t.Fatalf("EstimatedTokens(%q) = %d, want %d", test.text, got, test.want)
		}
	}
}

func TestEstimatedTokensCountsCJKPerRune(t *testing.T) {
	text := "你好世界" // 4 runes, one token each
	if got := EstimatedTokens(text); got != 4 {
		t.Fatalf("EstimatedTokens(%q) = %d, want 4", text, got)
	}
	mixed := "hi 你好"
	if got := EstimatedTokens(mixed); got != 3 { // 1 (hi) + 1 (space) + 8, rounded up
		t.Fatalf("EstimatedTokens(%q) = %d, want 3", mixed, got)
	}
	// Fullwidth forms and CJK punctuation are wide as well, so a full-width
	// query costs the same as the query it spells.
	if got := EstimatedTokens("ｎａｍｅ。"); got != 5 {
		t.Fatalf("EstimatedTokens(fullwidth) = %d, want 5", got)
	}
}

func TestTruncateToTokensIsRuneSafe(t *testing.T) {
	text := "你好世界"
	cut, truncated := TruncateToTokens(text, 2)
	if !truncated || cut != "你好" {
		t.Fatalf("TruncateToTokens = %q/%v, want 你好/true", cut, truncated)
	}
	if cut, truncated := TruncateToTokens(text, 10); truncated || cut != text {
		t.Fatalf("TruncateToTokens(10) = %q/%v, want full text", cut, truncated)
	}
	if cut, truncated := TruncateToTokens(text, 0); !truncated || cut != "" {
		t.Fatalf("TruncateToTokens(0) = %q/%v, want empty/true", cut, truncated)
	}
}

func TestContentTokensCountsEveryContentShape(t *testing.T) {
	if got := ContentTokens(coremessage.Content{}); got != 0 {
		t.Fatalf("empty content = %d, want 0", got)
	}
	// Parts without text cost one token, because a provider still has to
	// account for the payload.
	parts := coremessage.Content{Parts: []coremessage.Part{coremessage.ImagePart{}}}
	if got := ContentTokens(parts); got != 1 {
		t.Fatalf("textless parts = %d, want 1", got)
	}
	text := coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: "hello world"}}}
	if got := ContentTokens(text); got != EstimatedTokens("hello world") {
		t.Fatalf("text content = %d, want %d", got, EstimatedTokens("hello world"))
	}
}
