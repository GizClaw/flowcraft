package minimax

import "testing"

// TestNewTagsProvider locks down the sub-provider plumbing: minimax
// must call anthropic.LLM.WithProviderName so traces/metrics from
// MiniMax traffic land under "minimax" rather than the upstream
// "anthropic" tag.
func TestNewTagsProvider(t *testing.T) {
	c, err := New("MiniMax-M2.5", "k", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.Provider(); got != "minimax" {
		t.Fatalf("Provider() = %q, want minimax", got)
	}
}
