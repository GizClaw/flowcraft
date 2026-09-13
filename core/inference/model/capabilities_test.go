package model

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
)

func TestModelCapabilitiesValidate(t *testing.T) {
	capabilities := ModelCapabilities{
		Inputs:  []message.PartKind{message.PartText, message.PartImage},
		Outputs: []message.PartKind{message.PartText},
	}
	if err := capabilities.Validate(); err != nil {
		t.Fatalf("valid capabilities: %v", err)
	}

	for _, outputs := range [][]message.PartKind{
		{message.PartToolCall},
		{message.PartFile, message.PartImage},
		{message.PartText, message.PartText},
		{message.PartImage, message.PartText, message.PartImage},
	} {
		if err := (ModelCapabilities{Outputs: outputs}).Validate(); err == nil {
			t.Fatalf("outputs %v unexpectedly accepted", outputs)
		}
	}

	for _, inputs := range [][]message.PartKind{
		{message.PartText, message.PartText},
		{"unknown_kind"},
	} {
		if err := (ModelCapabilities{Inputs: inputs}).Validate(); err == nil {
			t.Fatalf("inputs %v unexpectedly accepted", inputs)
		}
	}

	for _, reasoning := range []ReasoningKind{
		ReasoningAlways,
		ReasoningToggle,
	} {
		if err := (ModelCapabilities{
			Reasoning: ReasoningCapability{Kind: reasoning},
		}).Validate(); err != nil {
			t.Fatalf("reasoning %q: %v", reasoning, err)
		}
	}
	if err := (ModelCapabilities{
		Reasoning: ReasoningCapability{Kind: "sometimes"},
	}).Validate(); err == nil {
		t.Fatal("unknown reasoning kind unexpectedly accepted")
	}
}

func TestModelCapabilitiesCloneDoesNotShareSlices(t *testing.T) {
	original := ModelCapabilities{
		Inputs:  []message.PartKind{message.PartText, message.PartImage},
		Outputs: []message.PartKind{message.PartText},
	}
	clone := original.Clone()
	clone.Inputs[0] = message.PartAudio
	clone.Outputs[0] = message.PartImage
	if original.Inputs[0] != message.PartText || original.Outputs[0] != message.PartText {
		t.Fatalf(
			"clone shares capability slices: original = %+v",
			original,
		)
	}
}

func TestModelCapabilitiesBuilders(t *testing.T) {
	capabilities := ModelCapabilities{}.
		WithInputs(message.PartText, message.PartImage).
		WithInputs(message.PartData).
		WithOutputs(message.PartText).
		WithReasoning(ReasoningAlways).
		WithHostedWebSearch()
	wantInputs := []message.PartKind{
		message.PartText,
		message.PartImage,
		message.PartData,
	}
	if !reflect.DeepEqual(capabilities.Inputs, wantInputs) {
		t.Fatalf("inputs = %v, want %v", capabilities.Inputs, wantInputs)
	}
	if !reflect.DeepEqual(capabilities.Outputs, []message.PartKind{message.PartText}) {
		t.Fatalf("outputs = %v", capabilities.Outputs)
	}
	if capabilities.Reasoning.Kind != ReasoningAlways ||
		!capabilities.HostedWebSearch {
		t.Fatalf("capabilities = %+v", capabilities)
	}
	if err := capabilities.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestCustomEmbedDimensionsLeaf(t *testing.T) {
	var capabilities ModelCapabilities
	if err := json.Unmarshal([]byte(
		`{"inputs":["text"],"custom_embed_dimensions":true}`), &capabilities); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !capabilities.CustomEmbedDimensions {
		t.Fatal("custom_embed_dimensions: true must set the capability")
	}
	if err := capabilities.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestModelCapabilitiesLegacyReasoningJSON(t *testing.T) {
	var capabilities ModelCapabilities
	if err := json.Unmarshal([]byte(`{
		"inputs": ["text"],
		"outputs": ["text"],
		"reasoning": "toggle"
	}`), &capabilities); err != nil {
		t.Fatalf("legacy capabilities decode: %v", err)
	}
	if capabilities.Reasoning.Kind != ReasoningToggle ||
		len(capabilities.Reasoning.EffortMap) != 0 {
		t.Fatalf("legacy capabilities decode = %+v", capabilities.Reasoning)
	}
	if err := capabilities.Validate(); err != nil {
		t.Fatalf("legacy capabilities Validate: %v", err)
	}
	encoded, err := json.Marshal(capabilities)
	if err != nil {
		t.Fatalf("legacy capabilities marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"reasoning":"toggle"`) {
		t.Fatalf("legacy capabilities marshal = %s, want string reasoning", encoded)
	}
}

func TestModelCapabilitiesBuildersDoNotAlias(t *testing.T) {
	base := ModelCapabilities{}.WithInputs(message.PartText)
	extended := base.WithInputs(message.PartImage)
	if len(base.Inputs) != 1 || base.Inputs[0] != message.PartText {
		t.Fatalf("base inputs mutated by builder: %v", base.Inputs)
	}
	_ = extended
}

func TestModelCapabilitiesCloneDoesNotAliasEffortMap(t *testing.T) {
	capabilities := ModelCapabilities{
		Reasoning: ReasoningCapability{
			Kind: ReasoningToggle,
			EffortMap: map[ReasoningEffort]string{
				ReasoningMinimal: "low",
				ReasoningLow:     "low",
				ReasoningMedium:  "high",
				ReasoningHigh:    "high",
				ReasoningXHigh:   "max",
			},
		},
	}
	clone := capabilities.Clone()
	clone.Reasoning.EffortMap[ReasoningLow] = "max"
	if got := capabilities.Reasoning.EffortMap[ReasoningLow]; got != "low" {
		t.Fatalf("clone mutated original effort map: low = %q, want low", got)
	}
}
