package azure

import "testing"

// TestNewTagsProvider locks down the sub-provider plumbing: azure must
// call openai.LLM.WithProviderName so traces/metrics from Azure-routed
// traffic land under "azure" rather than the upstream "openai" tag.
func TestNewTagsProvider(t *testing.T) {
	c, err := New("gpt-5", "k", "https://example.openai.azure.com", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.Provider(); got != "azure" {
		t.Fatalf("Provider() = %q, want azure", got)
	}
}
