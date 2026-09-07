package anthropic

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

func TestCatalogDeclaresMaxInputTokens(t *testing.T) {
	provider, err := buildProvider(context.Background(), ResourceSettings{ID: "anthropic"}, nil)
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

func TestCatalogDeclaresMaxOutputTokens(t *testing.T) {
	provider, err := buildProvider(context.Background(), ResourceSettings{ID: "anthropic"}, nil)
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
		if entry.maxOutputTokens <= 0 || descriptor.Limits.MaxOutputTokens == nil {
			t.Errorf("model %q: max output tokens not declared", name)
		}
	}
	checks := map[string]int{
		"claude-fable-5":    128_000,
		"claude-sonnet-5":   128_000,
		"claude-haiku-4-5":  64_000,
		"claude-sonnet-4-6": 128_000,
		"claude-opus-4-1":   32_000,
	}
	for name, want := range checks {
		descriptor, ok := descriptors[name]
		if !ok {
			t.Fatalf("model %q missing from provider", name)
		}
		if descriptor.Limits.MaxOutputTokens == nil ||
			*descriptor.Limits.MaxOutputTokens != want {
			t.Errorf("model %q: max output tokens = %v, want %d",
				name, descriptor.Limits.MaxOutputTokens, want)
		}
	}
}

func TestCatalogPublishesCapabilities(t *testing.T) {
	provider, err := buildProvider(context.Background(), ResourceSettings{ID: "anthropic"}, nil)
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	for _, model := range provider.Models {
		capabilities := model.Descriptor.Capabilities
		if !reflect.DeepEqual(capabilities.Outputs, []message.PartKind{message.PartText}) {
			t.Fatalf("%s outputs = %v, want text", model.Descriptor.ID.Name, capabilities.Outputs)
		}
		if !slices.Contains(capabilities.Inputs, message.PartImage) {
			t.Fatalf("%s inputs = %v, want image input", model.Descriptor.ID.Name, capabilities.Inputs)
		}
		want := inference.ReasoningToggle
		switch model.Descriptor.ID.Name {
		case "claude-fable-5", "claude-mythos-5":
			want = inference.ReasoningAlways
		}
		if capabilities.Reasoning.Kind != want {
			t.Fatalf("%s reasoning = %q, want %q",
				model.Descriptor.ID.Name, capabilities.Reasoning, want)
		}
	}
}

func TestMergedCatalogRejectsMissingTextOutput(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(
		`{"models":[{"name":"m","capabilities":{"inputs":["text"]}}]}`,
	))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	if _, err := mergedCatalog(spec); err == nil {
		t.Fatal("mergedCatalog unexpectedly accepted a model without text output")
	}
}

func TestMergedCatalogOverlaysDeclaredLimits(t *testing.T) {
	capabilities := `{"inputs":["text","data","tool_call","tool_result"],"outputs":["text"]}`
	spec, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "claude-sonnet-5",
			"capabilities": `+capabilities+`
		}]
	}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	entry := models["claude-sonnet-5"]
	if entry.maxInputTokens != 1_000_000 || entry.maxOutputTokens != 128_000 {
		t.Fatalf("redeclared limits = %d/%d, want catalog 1000000/128000",
			entry.maxInputTokens, entry.maxOutputTokens)
	}

	spec, err = decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "claude-sonnet-5",
			"capabilities": `+capabilities+`,
			"limits": {"max_output_tokens": 4096}
		}]
	}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err = mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	entry = models["claude-sonnet-5"]
	if entry.maxInputTokens != 1_000_000 || entry.maxOutputTokens != 4096 {
		t.Fatalf("overridden limits = %d/%d, want 1000000/4096",
			entry.maxInputTokens, entry.maxOutputTokens)
	}
}
