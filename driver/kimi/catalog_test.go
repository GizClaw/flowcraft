package kimi

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

func TestCatalogDeclaresMaxInputTokens(t *testing.T) {
	provider, err := buildProvider(context.Background(), ResourceSettings{ID: "kimi"}, nil)
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
		"kimi-k3":          1_000_000,
		"kimi-k2.7-code":   256_000,
		"moonshot-v1-8k":   8_192,
		"moonshot-v1-128k": 131_072,
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
	provider, err := buildProvider(context.Background(), ResourceSettings{ID: "kimi"}, nil)
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	descriptors := make(map[string]inference.ModelDescriptor, len(provider.Models))
	for _, model := range provider.Models {
		descriptors[model.Descriptor.ID.Name] = model.Descriptor
	}
	for name, entry := range catalog {
		if entry.maxOutputTokens <= 0 {
			continue // family entries without a documented ceiling stay nil.
		}
		descriptor := descriptors[name]
		if descriptor.Limits.MaxOutputTokens == nil ||
			*descriptor.Limits.MaxOutputTokens != entry.maxOutputTokens {
			t.Errorf("model %q: descriptor limit = %v, want %d",
				name, descriptor.Limits.MaxOutputTokens, entry.maxOutputTokens)
		}
	}
	checks := map[string]int{
		"kimi-k3":                  1_048_576,
		"kimi-k2.7-code":           131_072,
		"kimi-k2.7-code-highspeed": 131_072,
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
	provider, err := buildProvider(context.Background(), ResourceSettings{ID: "kimi"}, nil)
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	descriptors := make(map[string]inference.ModelDescriptor, len(provider.Models))
	for _, model := range provider.Models {
		descriptors[model.Descriptor.ID.Name] = model.Descriptor
	}

	k3 := descriptors["kimi-k3"]
	if !reflect.DeepEqual(k3.Capabilities.Outputs, []message.PartKind{message.PartText}) ||
		!slices.Contains(k3.Capabilities.Inputs, message.PartImage) ||
		!slices.Contains(k3.Capabilities.Inputs, message.PartVideo) ||
		k3.Capabilities.Reasoning.Kind != inference.ReasoningAlways {
		t.Fatalf("kimi-k3 capabilities = %+v", k3.Capabilities)
	}

	k26 := descriptors["kimi-k2.6"]
	if !slices.Contains(k26.Capabilities.Inputs, message.PartVideo) ||
		k26.Capabilities.Reasoning.Kind != inference.ReasoningToggle {
		t.Fatalf("kimi-k2.6 capabilities = %+v", k26.Capabilities)
	}

	k27code := descriptors["kimi-k2.7-code"]
	if !slices.Contains(k27code.Capabilities.Inputs, message.PartVideo) ||
		k27code.Capabilities.Reasoning.Kind != inference.ReasoningAlways {
		t.Fatalf("kimi-k2.7-code capabilities = %+v", k27code.Capabilities)
	}

	moonshot := descriptors["moonshot-v1-8k"]
	if moonshot.Capabilities.Reasoning.Kind != inference.ReasoningNone ||
		len(moonshot.Capabilities.Inputs) == 0 {
		t.Fatalf("moonshot-v1-8k capabilities = %+v", moonshot.Capabilities)
	}
}

func TestMergedCatalogRejectsMissingTextOutput(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(
		`{"models":[{"name":"m","kind":"generate"}]}`,
	))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	if _, err := mergedCatalog(spec); err == nil {
		t.Fatal("mergedCatalog unexpectedly accepted a generate model without text output")
	}
}

func TestMergedCatalogOverlaysDeclaredLimits(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "kimi-k3",
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
	entry := models["kimi-k3"]
	if entry.maxInputTokens != 1_000_000 || entry.maxOutputTokens != 1_048_576 {
		t.Fatalf("redeclared limits = %d/%d, want catalog 1000000/1048576",
			entry.maxInputTokens, entry.maxOutputTokens)
	}

	spec, err = decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "kimi-k3",
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
	entry = models["kimi-k3"]
	if entry.maxInputTokens != 1_000_000 || entry.maxOutputTokens != 4096 {
		t.Fatalf("overridden limits = %d/%d, want 1000000/4096",
			entry.maxInputTokens, entry.maxOutputTokens)
	}
}
