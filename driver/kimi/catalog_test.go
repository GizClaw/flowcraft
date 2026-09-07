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
		if entry.limits.MaxOutputTokens == nil {
			continue // family entries without a documented ceiling stay nil.
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
	in, out := entry.limits.Values()
	if in != 1_000_000 || out != 1_048_576 {
		t.Fatalf("redeclared limits = %d/%d, want catalog 1000000/1048576",
			in, out)
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
	in, out = entry.limits.Values()
	if in != 1_000_000 || out != 4096 {
		t.Fatalf("overridden limits = %d/%d, want 1000000/4096",
			in, out)
	}
}

func TestMergedCatalogOverridesDeclaredInputLimit(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "kimi-k3",
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
	entry := models["kimi-k3"]
	in, out := entry.limits.Values()
	if in != 65_536 || out != 1_048_576 {
		t.Fatalf("declared input limits = %d/%d, want 65536/1048576",
			in, out)
	}
}
