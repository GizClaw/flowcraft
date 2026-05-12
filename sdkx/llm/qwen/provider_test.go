package qwen

import "testing"

// TestNewTagsProvider locks down the sub-provider plumbing: the qwen
// wrapper must call openai.LLM.WithProviderName so traces/metrics from
// Qwen traffic land under "qwen" rather than the upstream "openai" tag.
// Regressing this silently aggregates Qwen QPS into the openai bucket,
// which is exactly the observability bug this plumbing exists to fix.
func TestNewTagsProvider(t *testing.T) {
	c, err := New("qwen-flash", "k", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.inner.Provider(); got != "qwen" {
		t.Fatalf("Provider() = %q, want qwen", got)
	}
}
