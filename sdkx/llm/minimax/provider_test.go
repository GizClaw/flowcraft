package minimax

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

// TestNewTagsProvider locks down the sub-provider plumbing: minimax
// must call anthropic.LLM.WithProviderName so traces/metrics from
// MiniMax traffic land under "minimax" rather than the upstream
// "anthropic" tag.
func TestNewTagsProvider(t *testing.T) {
	c, err := New("MiniMax-M2.7-highspeed", "k", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.Provider(); got != "minimax" {
		t.Fatalf("Provider() = %q, want minimax", got)
	}
}

func TestNewOpenAITagsProvider(t *testing.T) {
	c, err := NewOpenAI("MiniMax-M2.7-highspeed", "k", "")
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}
	if got := c.Provider(); got != "minimax-oai" {
		t.Fatalf("Provider() = %q, want minimax-oai", got)
	}
}

func TestOpenAICompatibleProviderRegistered(t *testing.T) {
	c, err := llm.NewFromConfig("minimax-oai", "MiniMax-M2.7-highspeed", map[string]any{
		"api_key": "k",
	})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	tagged, ok := c.(interface{ Provider() string })
	if !ok {
		t.Fatalf("client does not expose Provider()")
	}
	if got := tagged.Provider(); got != "minimax-oai" {
		t.Fatalf("Provider() = %q, want minimax-oai", got)
	}
}

func TestOpenAICompatibleCatalogDisablesJSONFormatting(t *testing.T) {
	spec := llm.DefaultRegistry.LookupModelSpec("minimax-oai", "MiniMax-M2.7-highspeed")
	if spec.Caps.Supports(llm.CapJSONSchema) {
		t.Fatal("minimax-oai M2.7 must not advertise JSON schema support")
	}
	if spec.Caps.Supports(llm.CapJSONMode) {
		t.Fatal("minimax-oai M2.7 must not advertise JSON mode support")
	}
}

func TestOpenAICompatibleCatalogDoesNotDefaultReasoningSplit(t *testing.T) {
	spec := llm.DefaultRegistry.LookupModelSpec("minimax-oai", "MiniMax-M2.7-highspeed")
	if _, ok := spec.Defaults.Extra["reasoning_split"]; ok {
		t.Fatalf("reasoning_split default = %v, want absent", spec.Defaults.Extra["reasoning_split"])
	}
}
