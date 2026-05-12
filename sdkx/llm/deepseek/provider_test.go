package deepseek

import "testing"

// TestNewTagsProvider locks down the sub-provider plumbing: deepseek
// must call openai.LLM.WithProviderName so traces/metrics from DeepSeek
// traffic land under "deepseek" rather than the upstream "openai" tag.
func TestNewTagsProvider(t *testing.T) {
	c, err := New("deepseek-chat", "k", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.Provider(); got != "deepseek" {
		t.Fatalf("Provider() = %q, want deepseek", got)
	}
}
