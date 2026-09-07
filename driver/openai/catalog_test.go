package openai

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

func TestCatalogDeclaresMaxInputTokens(t *testing.T) {
	provider, err := buildProvider(context.Background(), ResourceSettings{ID: "openai"}, nil)
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	descriptors := make(map[string]inference.ModelDescriptor, len(provider.Models))
	for _, model := range provider.Models {
		descriptors[model.Descriptor.ID.Name] = model.Descriptor
	}
	for name, entry := range catalog {
		if entry.kind != kindGenerate && entry.kind != kindEmbed {
			continue
		}
		descriptor, ok := descriptors[name]
		if !ok {
			t.Fatalf("catalog model %q missing from provider", name)
		}
		if descriptor.Limits.MaxInputTokens == nil {
			t.Errorf("model %q: max input tokens not declared", name)
		}
	}
	checks := map[string]int{
		"gpt-5.6-sol":            1_050_000,
		"gpt-5.4-mini":           400_000,
		"gpt-4.1":                1_047_576,
		"text-embedding-3-small": 8_192,
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
	provider, err := buildProvider(context.Background(), ResourceSettings{ID: "openai"}, nil)
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
		descriptor, ok := descriptors[name]
		if !ok {
			t.Fatalf("catalog model %q missing from provider", name)
		}
		if descriptor.Limits.MaxOutputTokens == nil {
			t.Errorf("model %q: max output tokens not declared", name)
		}
	}
	checks := map[string]int{
		"gpt-5.6-sol":  128_000,
		"gpt-5.4-mini": 128_000,
		"gpt-4.1":      32_768,
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
	provider, err := buildProvider(context.Background(), ResourceSettings{ID: "openai"}, nil)
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	descriptors := make(map[string]inference.ModelDescriptor, len(provider.Models))
	for _, model := range provider.Models {
		descriptors[model.Descriptor.ID.Name] = model.Descriptor
	}

	flagship := descriptors["gpt-5.6-sol"]
	if !reflect.DeepEqual(
		flagship.Capabilities.Outputs,
		[]message.PartKind{message.PartText},
	) {
		t.Fatalf("gpt-5.6-sol outputs = %v, want text", flagship.Capabilities.Outputs)
	}
	if !slices.Contains(flagship.Capabilities.Inputs, message.PartImage) {
		t.Fatalf("gpt-5.6-sol inputs = %v, want image input", flagship.Capabilities.Inputs)
	}
	if !flagship.Capabilities.HostedWebSearch {
		t.Fatal("gpt-5.6-sol must declare hosted web search")
	}
	if flagship.Capabilities.Reasoning.Kind != inference.ReasoningToggle {
		t.Fatalf(
			"gpt-5.6-sol reasoning = %q, want toggle",
			flagship.Capabilities.Reasoning,
		)
	}

	nano := descriptors["gpt-4.1-nano"]
	if nano.Capabilities.HostedWebSearch {
		t.Fatal("gpt-4.1-nano must not declare hosted web search")
	}
	if nano.Lifecycle.Status != inference.ModelStatusDeprecated ||
		nano.Lifecycle.Replacement == nil ||
		nano.Lifecycle.Replacement.Name != "gpt-5.6-luna" {
		t.Fatalf("gpt-4.1-nano lifecycle = %+v", nano.Lifecycle)
	}
	if !slices.Contains(nano.Capabilities.Inputs, message.PartImage) {
		t.Fatalf("gpt-4.1-nano inputs = %v, want image input", nano.Capabilities.Inputs)
	}
	if nano.Capabilities.Reasoning.Kind != inference.ReasoningNone {
		t.Fatalf(
			"gpt-4.1-nano reasoning = %q, want none",
			nano.Capabilities.Reasoning,
		)
	}

	image := descriptors["gpt-image-2"]
	if !reflect.DeepEqual(
		image.Capabilities.Outputs,
		[]message.PartKind{message.PartImage},
	) || !reflect.DeepEqual(
		image.Capabilities.Inputs,
		[]message.PartKind{message.PartText, message.PartImage},
	) {
		t.Fatalf("gpt-image-2 capabilities = %+v", image.Capabilities)
	}

	tts := descriptors["gpt-4o-mini-tts"]
	if !reflect.DeepEqual(
		tts.Capabilities.Outputs,
		[]message.PartKind{message.PartAudio},
	) {
		t.Fatalf("gpt-4o-mini-tts outputs = %v, want audio", tts.Capabilities.Outputs)
	}

	embed := descriptors["text-embedding-3-small"]
	if len(embed.Capabilities.Outputs) != 0 {
		t.Fatalf("embed model outputs = %v, want none", embed.Capabilities.Outputs)
	}
}

func TestMergedCatalogRejectsFamilyContractViolation(t *testing.T) {
	cases := []struct {
		name string
		spec string
	}{
		{"image without image output",
			`{"models":[{"name":"m","kind":"image","capabilities":{"outputs":["text"]}}]}`},
		{"generate without text output",
			`{"models":[{"name":"m","kind":"generate"}]}`},
		{"tts without audio output",
			`{"models":[{"name":"m","kind":"tts","capabilities":{"outputs":["text"]}}]}`},
		{"embed with generate output",
			`{"models":[{"name":"m","kind":"embed","capabilities":{"outputs":["text"]}}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := decodeSpec(context.Background(), []byte(tc.spec))
			if err != nil {
				t.Fatalf("decodeSpec: %v", err)
			}
			if _, err := mergedCatalog(spec); err == nil {
				t.Fatal("mergedCatalog unexpectedly accepted contract violation")
			}
		})
	}
}

func TestMergedCatalogAppliesChatStreamUsagePolicy(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(
		`{"api":"chat","chat_stream_options":{"include_usage":false,"include_obfuscation":false}}`,
	))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	if models["gpt-5.6-sol"].includeChatStreamUsage() {
		t.Fatal("chat_stream_options include_usage=false must reach catalog entries")
	}
	if obfuscation := models["gpt-5.6-sol"].chatStreamObfuscation(); obfuscation == nil || *obfuscation {
		t.Fatal("chat_stream_options include_obfuscation=false must reach catalog entries")
	}

	spec, err = decodeSpec(context.Background(), []byte(`{"api":"chat"}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err = mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	if !models["gpt-5.6-sol"].includeChatStreamUsage() {
		t.Fatal("nil chat_stream_options must keep the driver default of true")
	}
	if obfuscation := models["gpt-5.6-sol"].chatStreamObfuscation(); obfuscation != nil {
		t.Fatal("nil chat_stream_options must keep the OpenAI default obfuscation policy")
	}
}

func TestMergedCatalogOverlaysDeclaredLimits(t *testing.T) {
	generateCapabilities := `{"inputs":["text","data","tool_call","tool_result"],"outputs":["text"]}`
	spec, err := decodeSpec(context.Background(), []byte(`{
		"models": [
			{
				"name": "custom",
				"kind": "generate",
				"capabilities": `+generateCapabilities+`,
				"limits": {"max_input_tokens": 100, "max_output_tokens": 200}
			},
			{
				"name": "gpt-4.1",
				"kind": "generate",
				"capabilities": `+generateCapabilities+`
			}
		]
	}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	custom := models["custom"]
	if custom.maxInputTokens != 100 || custom.maxOutputTokens != 200 {
		t.Fatalf("custom limits = %d/%d, want 100/200",
			custom.maxInputTokens, custom.maxOutputTokens)
	}
	// Redeclaring a built-in model without limits keeps the catalog values.
	builtin := models["gpt-4.1"]
	if builtin.maxInputTokens != 1_047_576 || builtin.maxOutputTokens != 32_768 {
		t.Fatalf("gpt-4.1 limits = %d/%d, want catalog 1047576/32768",
			builtin.maxInputTokens, builtin.maxOutputTokens)
	}

	spec, err = decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "gpt-4.1",
			"kind": "generate",
			"capabilities": `+generateCapabilities+`,
			"limits": {"max_output_tokens": 1000}
		}]
	}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err = mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	overridden := models["gpt-4.1"]
	if overridden.maxInputTokens != 1_047_576 {
		t.Fatalf("gpt-4.1 max input = %d, want built-in 1047576",
			overridden.maxInputTokens)
	}
	if overridden.maxOutputTokens != 1000 {
		t.Fatalf("gpt-4.1 max output = %d, want declared 1000",
			overridden.maxOutputTokens)
	}
}

func TestMergedCatalogOverridesDeclaredInputLimit(t *testing.T) {
	generateCapabilities := `{"inputs":["text","data","tool_call","tool_result"],"outputs":["text"]}`
	spec, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "gpt-4.1",
			"kind": "generate",
			"capabilities": `+generateCapabilities+`,
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
	entry := models["gpt-4.1"]
	if entry.maxInputTokens != 65_536 || entry.maxOutputTokens != 32_768 {
		t.Fatalf("declared input limits = %d/%d, want 65536/32768",
			entry.maxInputTokens, entry.maxOutputTokens)
	}
}
