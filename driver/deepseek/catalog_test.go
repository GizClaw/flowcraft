package deepseek

import (
	"testing"
)

func TestCatalogDeclaresMaxInputTokens(t *testing.T) {
	provider, err := buildProvider(ResourceSettings{ID: "deepseek"})
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	for _, model := range provider.Models {
		name := model.Descriptor.ID.Name
		if catalog[name].maxInputTokens <= 0 {
			t.Errorf("model %q: max input tokens not declared", name)
		}
		if model.Descriptor.Limits.MaxInputTokens == nil ||
			*model.Descriptor.Limits.MaxInputTokens != 1_000_000 {
			t.Errorf("model %q: max input tokens = %v, want 1000000",
				name, model.Descriptor.Limits.MaxInputTokens)
		}
	}
}
