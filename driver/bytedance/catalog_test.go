package bytedance

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

func TestCatalogDeclaresMaxInputTokens(t *testing.T) {
	provider, err := buildProvider(context.Background(), ResourceSettings{ID: "bytedance"})
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
		if entry.maxInputTokens <= 0 || descriptor.Limits.MaxInputTokens == nil {
			t.Errorf("model %q: max input tokens not declared", name)
		}
	}
	checks := map[string]int{
		"doubao-seed-evolving":    1_024_000,
		"doubao-seed-2-1-pro":     256_000,
		"doubao-seed-1-6-vision":  256_000,
		"doubao-embedding-large":  4_095,
		"doubao-embedding-vision": 8_191,
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

func TestCatalogPublishesCapabilities(t *testing.T) {
	provider, err := buildProvider(context.Background(), ResourceSettings{ID: "bytedance"})
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	descriptors := make(map[string]inference.ModelDescriptor, len(provider.Models))
	for _, model := range provider.Models {
		descriptors[model.Descriptor.ID.Name] = model.Descriptor
	}

	chat := descriptors["doubao-seed-2-1-pro"]
	if !reflect.DeepEqual(chat.Capabilities.Outputs, []message.PartKind{message.PartText}) {
		t.Fatalf("chat outputs = %v, want text", chat.Capabilities.Outputs)
	}
	if !slices.Contains(chat.Capabilities.Inputs, message.PartImage) ||
		!slices.Contains(chat.Capabilities.Inputs, message.PartVideo) {
		t.Fatalf("chat inputs = %v, want image and video input", chat.Capabilities.Inputs)
	}
	if !chat.Capabilities.HostedWebSearch ||
		chat.Capabilities.Reasoning != inference.ReasoningToggle {
		t.Fatalf("chat capabilities = %+v", chat.Capabilities)
	}

	image := descriptors["doubao-seedream-5-0"]
	if !reflect.DeepEqual(image.Capabilities.Outputs, []message.PartKind{message.PartImage}) ||
		!reflect.DeepEqual(
			image.Capabilities.Inputs,
			[]message.PartKind{message.PartText, message.PartImage},
		) {
		t.Fatalf("image capabilities = %+v", image.Capabilities)
	}

	video := descriptors["doubao-seedance-2-0"]
	if !reflect.DeepEqual(video.Capabilities.Outputs, []message.PartKind{message.PartVideo}) {
		t.Fatalf("video outputs = %v, want video", video.Capabilities.Outputs)
	}
	if !slices.Contains(video.Capabilities.Inputs, message.PartVideo) ||
		!slices.Contains(video.Capabilities.Inputs, message.PartAudio) {
		t.Fatalf(
			"seedance 2.0 inputs = %v, want video/audio reference input",
			video.Capabilities.Inputs,
		)
	}
	if !video.Capabilities.HostedWebSearch {
		t.Fatal("seedance 2.0 must declare hosted web search")
	}
	flagship := descriptors["doubao-seedance-2-5"]
	if !flagship.Capabilities.HostedWebSearch {
		t.Fatal("seedance 2.5 must declare hosted web search")
	}
	legacy := descriptors["doubao-seedance-1-5-pro"]
	if slices.Contains(legacy.Capabilities.Inputs, message.PartVideo) ||
		slices.Contains(legacy.Capabilities.Inputs, message.PartAudio) ||
		legacy.Capabilities.HostedWebSearch {
		t.Fatalf(
			"seedance 1.5 pro capabilities = %+v, want no reference/web-search capabilities",
			legacy.Capabilities,
		)
	}

	embed := descriptors["doubao-embedding-vision"]
	if !slices.Contains(embed.Capabilities.Inputs, message.PartImage) {
		t.Fatalf("multimodal embed inputs = %v, want image input", embed.Capabilities.Inputs)
	}
	if len(embed.Capabilities.Outputs) != 0 {
		t.Fatalf("embed outputs = %v, want none", embed.Capabilities.Outputs)
	}
}

func TestCatalogDeclaresAudioInput(t *testing.T) {
	provider, err := buildProvider(context.Background(), ResourceSettings{ID: "bytedance"})
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	descriptors := make(map[string]inference.ModelDescriptor, len(provider.Models))
	for _, model := range provider.Models {
		descriptors[model.Descriptor.ID.Name] = model.Descriptor
	}
	// Ark's audio-understanding line is the 260428 lite/mini revisions;
	// the rest of the generate family does not take audio input.
	checks := map[string]bool{
		"doubao-seed-2-0-lite": true,
		"doubao-seed-2-0-mini": true,
		"doubao-seed-2-0-pro":  false,
		"doubao-seed-2-1-pro":  false,
		"doubao-seed-evolving": false,
	}
	for name, want := range checks {
		descriptor, ok := descriptors[name]
		if !ok {
			t.Fatalf("model %q missing from provider", name)
		}
		if has := slices.Contains(descriptor.Capabilities.Inputs, message.PartAudio); has != want {
			t.Errorf("model %q: audio input = %v, want %v", name, has, want)
		}
	}
}

func TestMergedCatalogRejectsFamilyContractViolation(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(
		`{"models":[{"name":"m","kind":"image","capabilities":{"outputs":["text"]}}]}`,
	))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	if _, err := mergedCatalog(spec); err == nil {
		t.Fatal("mergedCatalog unexpectedly accepted contract violation")
	}
}
