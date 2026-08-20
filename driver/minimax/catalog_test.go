package minimax

import (
	"reflect"
	"slices"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
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

func TestCatalogPublishesCapabilities(t *testing.T) {
	provider, err := buildProvider(ResourceSettings{ID: "minimax"})
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	descriptors := make(map[string]inference.ModelDescriptor, len(provider.Models))
	for _, model := range provider.Models {
		descriptors[model.Descriptor.ID.Name] = model.Descriptor
	}

	m3 := descriptors["MiniMax-M3"]
	if !reflect.DeepEqual(m3.Capabilities.Outputs, []message.PartKind{message.PartText}) ||
		!slices.Contains(m3.Capabilities.Inputs, message.PartImage) ||
		m3.Capabilities.Reasoning != inference.ReasoningToggle {
		t.Fatalf("M3 capabilities = %+v", m3.Capabilities)
	}

	m2 := descriptors["MiniMax-M2.7"]
	if m2.Capabilities.Reasoning != inference.ReasoningAlways {
		t.Fatalf("M2.7 reasoning = %q, want always", m2.Capabilities.Reasoning)
	}

	image := descriptors["image-01"]
	if !reflect.DeepEqual(image.Capabilities.Outputs, []message.PartKind{message.PartImage}) ||
		!reflect.DeepEqual(
			image.Capabilities.Inputs,
			[]message.PartKind{message.PartText, message.PartImage},
		) {
		t.Fatalf("image capabilities = %+v", image.Capabilities)
	}

	video := descriptors["MiniMax-Hailuo-2.3"]
	if !reflect.DeepEqual(video.Capabilities.Outputs, []message.PartKind{message.PartVideo}) {
		t.Fatalf("video outputs = %v, want video", video.Capabilities.Outputs)
	}

	tts := descriptors["speech-2.8-hd"]
	if !reflect.DeepEqual(tts.Capabilities.Outputs, []message.PartKind{message.PartAudio}) {
		t.Fatalf("tts outputs = %v, want audio", tts.Capabilities.Outputs)
	}

	music := descriptors["music-3.0"]
	if !reflect.DeepEqual(music.Capabilities.Outputs, []message.PartKind{message.PartAudio}) {
		t.Fatalf("music outputs = %v, want audio", music.Capabilities.Outputs)
	}
}

func TestMergedCatalogRejectsFamilyContractViolation(t *testing.T) {
	spec, err := decodeSpec([]byte(
		`{"models":[{"name":"m","kind":"video","capabilities":{"outputs":["text"]}}]}`,
	))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	if _, err := mergedCatalog(spec); err == nil {
		t.Fatal("mergedCatalog unexpectedly accepted contract violation")
	}
}
