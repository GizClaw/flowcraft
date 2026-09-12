package model

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
)

func reasoningPatchCapabilities() ModelCapabilities {
	return ModelCapabilities{
		Inputs: []message.PartKind{
			message.PartText,
			message.PartImage,
			message.PartToolCall,
			message.PartToolResult,
		},
		Outputs: []message.PartKind{message.PartText},
		Reasoning: ReasoningCapability{
			Kind: ReasoningToggle,
			EffortMap: map[ReasoningEffort]string{
				ReasoningMinimal: "minimal",
				ReasoningLow:     "low",
				ReasoningMedium:  "medium",
				ReasoningHigh:    "high",
				ReasoningXHigh:   "xhigh",
			},
		},
		HostedWebSearch: true,
	}
}

func decodePatch(t *testing.T, raw string) *CapabilitiesPatch {
	t.Helper()
	var patch CapabilitiesPatch
	if err := json.Unmarshal([]byte(raw), &patch); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	if err := patch.Validate(); err != nil {
		t.Fatalf("validate patch: %v", err)
	}
	return &patch
}

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

func TestCapabilitiesPatchAbsentLeavesInherit(t *testing.T) {
	patch := decodePatch(t, `{"inputs":["text"],"hosted_web_search":false}`)
	got := patch.Apply(reasoningPatchCapabilities())
	if len(got.Inputs) != 1 || got.Inputs[0] != message.PartText {
		t.Fatalf("inputs = %v, want text only", got.Inputs)
	}
	if got.HostedWebSearch {
		t.Fatal("hosted_web_search: false must replace the base true")
	}
	if got.Reasoning.Kind != ReasoningToggle {
		t.Fatalf("reasoning kind = %q, want inherited toggle", got.Reasoning.Kind)
	}
	if len(got.Reasoning.EffortMap) != 5 {
		t.Fatalf("reasoning effort map must be inherited, got %v", got.Reasoning.EffortMap)
	}
	if len(got.Outputs) != 1 || got.Outputs[0] != message.PartText {
		t.Fatalf("outputs = %v, want inherited text", got.Outputs)
	}
}

func TestCapabilitiesPatchWholeDeclarationOverZeroBase(t *testing.T) {
	patch := decodePatch(t, `{"outputs":["text"],"hosted_web_search":true}`)
	got := patch.Apply(ModelCapabilities{})
	if len(got.Inputs) != 0 {
		t.Fatalf("inputs = %v, want conservative zero", got.Inputs)
	}
	if len(got.Outputs) != 1 || got.Outputs[0] != message.PartText {
		t.Fatalf("outputs = %v, want text", got.Outputs)
	}
	if !got.HostedWebSearch {
		t.Fatal("hosted web search must be declared")
	}
	if got.Reasoning.Kind != ReasoningNone {
		t.Fatalf("reasoning = %q, want conservative none", got.Reasoning.Kind)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("merged capabilities invalid: %v", err)
	}
}

func TestCapabilitiesPatchValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"duplicate inputs", `{"inputs":["text","text"]}`, "duplicate input"},
		{"invalid output modality", `{"outputs":["tool_call"]}`, "not a representable output modality"},
		{"invalid kind", `{"reasoning":{"kind":"maybe"}}`, "unknown reasoning kind"},
		{"none with map", `{"reasoning":{"kind":"","effort_map":{"minimal":"minimal","low":"low","medium":"medium","high":"high","xhigh":"xhigh"}}}`, "cannot declare an effort map"},
		{"map misses level", `{"reasoning":{"kind":"toggle","effort_map":{"low":"low"}}}`, "misses canonical level"},
		{"non-canonical key", `{"reasoning":{"kind":"toggle","effort_map":{"low":"low","medium":"medium","high":"high","minimal":"minimal","xhigh":"xhigh","none":"none"}}}`, "non-canonical key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var patch CapabilitiesPatch
			if err := json.Unmarshal([]byte(tc.raw), &patch); err != nil {
				t.Fatalf("decode: %v", err)
			}
			err := patch.Validate()
			if err == nil {
				t.Fatal("patch unexpectedly valid")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err, tc.want)
			}
		})
	}
}

func TestCapabilitiesPatchEmptyLeafIsExplicit(t *testing.T) {
	var patch CapabilitiesPatch
	if err := json.Unmarshal([]byte(`{"inputs":[]}`), &patch); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if patch.Inputs == nil || len(*patch.Inputs) != 0 {
		t.Fatalf("inputs = %#v, want explicit empty list", patch.Inputs)
	}
	got := patch.Apply(reasoningPatchCapabilities())
	if len(got.Inputs) != 0 {
		t.Fatalf("inputs = %v, want cleared", got.Inputs)
	}
}

func TestCustomEmbedDimensionsLeaf(t *testing.T) {
	base := reasoningPatchCapabilities()
	if base.CustomEmbedDimensions {
		t.Fatal("test base unexpectedly declares custom embed dimensions")
	}
	on := decodePatch(t, `{"custom_embed_dimensions":true}`).Apply(base)
	if !on.CustomEmbedDimensions {
		t.Fatal("custom_embed_dimensions: true must set the capability")
	}
	declared := reasoningPatchCapabilities()
	declared.CustomEmbedDimensions = true
	off := decodePatch(t, `{"custom_embed_dimensions":false}`).Apply(declared)
	if off.CustomEmbedDimensions {
		t.Fatal("custom_embed_dimensions: false must clear the base capability")
	}
	inherited := decodePatch(t, `{"outputs":["text"]}`).Apply(declared)
	if !inherited.CustomEmbedDimensions {
		t.Fatal("absent leaf must inherit the base capability")
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
