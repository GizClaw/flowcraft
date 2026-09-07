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
	provider, err := buildProvider(context.Background(), ResourceSettings{ID: "qwen"}, nil)
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	descriptors := make(map[string]inference.ModelDescriptor, len(provider.Models))
	for _, model := range provider.Models {
		descriptors[model.Descriptor.ID.Name] = model.Descriptor
	}
	for name, entry := range catalog {
		if entry.limits.MaxInputTokens == nil {
			t.Errorf("model %q: max input tokens not declared", name)
		}
		descriptor := descriptors[name]
		in, _ := entry.limits.Values()
		din, _ := descriptor.Limits.Values()
		if din != in {
			t.Errorf("model %q: descriptor limit = %v, want %d",
				name, descriptor.Limits.MaxInputTokens, in)
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

func TestCatalogDeclaresMaxOutputTokens(t *testing.T) {
	provider, err := buildProvider(context.Background(), ResourceSettings{ID: "qwen"}, nil)
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
		if entry.limits.MaxOutputTokens == nil {
			t.Errorf("model %q: max output tokens not declared", name)
		}
		descriptor := descriptors[name]
		_, out := entry.limits.Values()
		_, dout := descriptor.Limits.Values()
		if dout != out {
			t.Errorf("model %q: descriptor limit = %v, want %d",
				name, descriptor.Limits.MaxOutputTokens, out)
		}
	}
	checks := map[string]int{
		"qwen3.8-max-preview": 131_072,
		"qwen3.7-flash":       131_072,
		"qwen3-vl-flash":      32_768,
		"qwen-turbo":          16_384,
		"qwen-max":            8_192,
	}
	for name, want := range checks {
		descriptor := descriptors[name]
		if descriptor.Limits.MaxOutputTokens == nil ||
			*descriptor.Limits.MaxOutputTokens != want {
			t.Errorf("model %q: max output tokens = %v, want %d",
				name, descriptor.Limits.MaxOutputTokens, want)
		}
	}
}

func TestCatalogPublishesCapabilities(t *testing.T) {
	provider, err := buildProvider(context.Background(), ResourceSettings{ID: "qwen"}, nil)
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
		thinking.Capabilities.Reasoning.Kind != inference.ReasoningAlways {
		t.Fatalf("thinking model capabilities = %+v", thinking.Capabilities)
	}

	plain := descriptors["qwen-plus"]
	if plain.Capabilities.Reasoning.Kind != inference.ReasoningNone ||
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

func TestMergedCatalogOverlaysDeclaredLimits(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "qwen3.7-flash",
			"capabilities": {"inputs":["text"],"outputs":["text"]}
		}]
	}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	entry := models["qwen3.7-flash"]
	in, out := entry.limits.Values()
	if in != 991_808 || out != 131_072 {
		t.Fatalf("redeclared limits = %d/%d, want catalog 991808/131072",
			in, out)
	}

	spec, err = decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "qwen3.7-flash",
			"capabilities": {"inputs":["text"],"outputs":["text"]},
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
	entry = models["qwen3.7-flash"]
	in, out = entry.limits.Values()
	if in != 991_808 || out != 4096 {
		t.Fatalf("overridden limits = %d/%d, want 991808/4096",
			in, out)
	}
}

func TestMergedCatalogOverridesDeclaredInputLimit(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "qwen3.7-flash",
			"capabilities": {"inputs":["text"],"outputs":["text"]},
			"limits": {"max_input_tokens": 65536}
		}]
	}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	entry := models["qwen3.7-flash"]
	in, out := entry.limits.Values()
	if in != 65_536 || out != 131_072 {
		t.Fatalf("declared input limits = %d/%d, want 65536/131072",
			in, out)
	}
}
