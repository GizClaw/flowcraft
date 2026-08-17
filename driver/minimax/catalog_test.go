package minimax

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

func TestCatalogDeclaresMaxInputTokens(t *testing.T) {
	provider, err := buildProvider(ResourceSettings{ID: "minimax"})
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	descriptors := make(map[string]inference.ModelDescriptor, len(provider.Models))
	for _, model := range provider.Models {
		descriptors[model.Descriptor.ID.Name] = model.Descriptor
	}
	for name, entry := range catalog {
		if entry.kind != kindGenerate {
			continue
		}
		descriptor := descriptors[name]
		if entry.maxInputTokens <= 0 || descriptor.Limits.MaxInputTokens == nil {
			t.Errorf("model %q: max input tokens not declared", name)
		}
	}
	checks := map[string]int{
		"MiniMax-M3":             1_000_000,
		"MiniMax-M2.7":           204_800,
		"MiniMax-M2.5-highspeed": 204_800,
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
