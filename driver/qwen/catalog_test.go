package qwen

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

func TestCatalogDeclaresMaxInputTokens(t *testing.T) {
	provider, err := buildProvider(ResourceSettings{ID: "qwen"})
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	descriptors := make(map[string]inference.ModelDescriptor, len(provider.Models))
	for _, model := range provider.Models {
		descriptors[model.Descriptor.ID.Name] = model.Descriptor
	}
	for name, entry := range catalog {
		if entry.maxInputTokens <= 0 {
			t.Errorf("model %q: max input tokens not declared", name)
		}
		descriptor := descriptors[name]
		if descriptor.Limits.MaxInputTokens == nil ||
			*descriptor.Limits.MaxInputTokens != entry.maxInputTokens {
			t.Errorf("model %q: descriptor limit = %v, want %d",
				name, descriptor.Limits.MaxInputTokens, entry.maxInputTokens)
		}
	}
	checks := map[string]int{
		"qwen3.7-max":       991_808,
		"qwen3-vl-plus":     260_096,
		"qwen-max":          30_720,
		"text-embedding-v4": 8_192,
	}
	for name, want := range checks {
		descriptor := descriptors[name]
		if descriptor.Limits.MaxInputTokens == nil ||
			*descriptor.Limits.MaxInputTokens != want {
			t.Errorf("model %q: max input tokens = %v, want %d",
				name, descriptor.Limits.MaxInputTokens, want)
		}
	}
}
