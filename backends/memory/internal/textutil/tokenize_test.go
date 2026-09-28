package textutil

import "testing"

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
