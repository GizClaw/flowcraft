package anthropic

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

func TestCatalogDeclaresMaxInputTokens(t *testing.T) {
	provider, err := buildProvider(ResourceSettings{ID: "anthropic"})
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	descriptors := make(map[string]inference.ModelDescriptor, len(provider.Models))
	for _, model := range provider.Models {
		descriptors[model.Descriptor.ID.Name] = model.Descriptor
	}
	for name, entry := range catalog {
		descriptor, ok := descriptors[name]
		if !ok {
			t.Fatalf("catalog model %q missing from provider", name)
		}
		if entry.maxInputTokens <= 0 || descriptor.Limits.MaxInputTokens == nil {
			t.Errorf("model %q: max input tokens not declared", name)
		}
	}
	checks := map[string]int{
		"claude-opus-5":     1_000_000,
		"claude-haiku-4-5":  200_000,
		"claude-sonnet-4-6": 1_000_000,
		"claude-opus-4-1":   200_000,
	}
	for name, want := range checks {
		descriptor, ok := descriptors[name]
		if !ok {
			t.Fatalf("model %q missing from provider", name)
		}
		if descriptor.Limits.MaxInputTokens == nil ||
			*descriptor.Limits.MaxInputTokens != want {
			t.Errorf("model %q: max input tokens = %v, want %d",
				name, descriptor.Limits.MaxInputTokens, want)
		}
	}
}
