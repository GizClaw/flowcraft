package deepseek

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

// TestNewTagsProvider locks down the sub-provider plumbing: deepseek
// must call openai.LLM.WithProviderName so traces/metrics from DeepSeek
// traffic land under "deepseek" rather than the upstream "openai" tag.
func TestNewTagsProvider(t *testing.T) {
	c, err := New("deepseek-v4-flash", "k", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.Provider(); got != "deepseek" {
		t.Fatalf("Provider() = %q, want deepseek", got)
	}
}

func TestCatalogDefaultsDisableThinking(t *testing.T) {
	for _, model := range []string{"deepseek-v4-flash", "deepseek-v4-pro"} {
		spec := llm.DefaultRegistry.LookupModelSpec("deepseek", model)
		thinking, ok := spec.Defaults.Extra["thinking"].(map[string]any)
		if !ok {
			t.Fatalf("%s thinking default = %#v, want object", model, spec.Defaults.Extra["thinking"])
		}
		if got := thinking["type"]; got != "disabled" {
			t.Fatalf("%s thinking.type = %#v, want disabled", model, got)
		}
	}
}
