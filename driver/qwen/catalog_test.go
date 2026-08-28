package qwen

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

func TestCatalogDeclaresMaxInputTokens(t *testing.T) {
	provider, err := buildProvider(context.Background(), ResourceSettings{ID: "qwen"})
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

func TestCatalogPublishesCapabilities(t *testing.T) {
	provider, err := buildProvider(context.Background(), ResourceSettings{ID: "qwen"})
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	descriptors := make(map[string]inference.ModelDescriptor, len(provider.Models))
	for _, model := range provider.Models {
		descriptors[model.Descriptor.ID.Name] = model.Descriptor
	}

	thinking := descriptors["qwen3.8-max-preview"]
	if !reflect.DeepEqual(thinking.Capabilities.Outputs, []message.PartKind{message.PartText}) ||
		!slices.Contains(thinking.Capabilities.Inputs, message.PartImage) ||
		!slices.Contains(thinking.Capabilities.Inputs, message.PartVideo) ||
		thinking.Capabilities.Reasoning != inference.ReasoningAlways {
		t.Fatalf("thinking model capabilities = %+v", thinking.Capabilities)
	}

	plain := descriptors["qwen-plus"]
	if plain.Capabilities.Reasoning != inference.ReasoningNone ||
		len(plain.Capabilities.Inputs) == 0 {
		t.Fatalf("plain model capabilities = %+v", plain.Capabilities)
	}

	embed := descriptors["qwen3-vl-embedding"]
	if !slices.Contains(embed.Capabilities.Inputs, message.PartImage) ||
		len(embed.Capabilities.Outputs) != 0 {
		t.Fatalf("multimodal embed capabilities = %+v", embed.Capabilities)
	}
}

func TestMergedCatalogRejectsEmbedReasoning(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(
		`{"models":[{"name":"m","kind":"embed","capabilities":{"reasoning":"toggle","inputs":["text"]}}]}`,
	))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	if _, err := mergedCatalog(spec); err == nil {
		t.Fatal("mergedCatalog unexpectedly accepted an embed model with reasoning")
	}
}
