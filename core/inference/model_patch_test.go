package inference

import (
	"encoding/json"
	"maps"
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

func decodeReasoningPatch(t *testing.T, raw string) *ReasoningPatch {
	t.Helper()
	var patch ReasoningPatch
	if err := json.Unmarshal([]byte(raw), &patch); err != nil {
		t.Fatalf("decode reasoning patch: %v", err)
	}
	if err := patch.Validate(); err != nil {
		t.Fatalf("validate reasoning patch: %v", err)
	}
	return &patch
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

func TestReasoningPatchSubLeavesAndLegacyString(t *testing.T) {
	base := reasoningPatchCapabilities().Reasoning

	kindOnly := decodeReasoningPatch(t, `"toggle"`)
	got := kindOnly.Apply(base)
	if got.Kind != ReasoningToggle || len(got.EffortMap) != 5 {
		t.Fatalf("legacy string must keep base map, got kind %q map %v", got.Kind, got.EffortMap)
	}

	mapOnly := decodeReasoningPatch(t, `{"effort_map":{}}`)
	got = mapOnly.Apply(base)
	if got.Kind != ReasoningToggle {
		t.Fatalf("kind = %q, want inherited toggle", got.Kind)
	}
	if len(got.EffortMap) != 0 {
		t.Fatalf("effort_map: {} must clear the base dial, got %v", got.EffortMap)
	}

	nonePatch := decodeReasoningPatch(t, `""`)
	if nonePatch.Kind == nil || *nonePatch.Kind != ReasoningNone {
		t.Fatalf("legacy empty string must mean kind none, got %#v", nonePatch.Kind)
	}
	got = nonePatch.Apply(base)
	if got.Kind != ReasoningNone || len(got.EffortMap) != 5 {
		t.Fatalf("none patch = %#v, want kind none with inherited map", got)
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

func TestReasoningCapabilityEmptyMapSemantics(t *testing.T) {
	empty := map[ReasoningEffort]string{}
	for name, capability := range map[string]ReasoningCapability{
		"none without map":    {Kind: ReasoningNone},
		"none with empty map": {Kind: ReasoningNone, EffortMap: empty},
		"toggle with empty map": {
			Kind:      ReasoningToggle,
			EffortMap: empty,
		},
	} {
		if err := capability.Validate(); err != nil {
			t.Fatalf("%s unexpectedly invalid: %v", name, err)
		}
	}
}

func TestModelLimitsBuilders(t *testing.T) {
	got := (ModelLimits{}).
		WithMaxInputTokens(1_000_000).
		WithMaxOutputTokens(128_000)
	if in, out := got.Values(); in != 1_000_000 || out != 128_000 {
		t.Fatalf("values = %d/%d, want 1000000/128000", in, out)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	zero := (ModelLimits{}).WithMaxInputTokens(0)
	if in, _ := zero.Values(); in != 0 {
		t.Fatalf("non-positive input must stay undeclared, got %d", in)
	}
	one := (ModelLimits{}).WithMaxInputTokens(1)
	if in, _ := one.Values(); in != 1 {
		t.Fatalf("positive input must be declared, got %d", in)
	}
}

func TestReasoningPatchJSONRoundTrip(t *testing.T) {
	for _, raw := range []string{
		`"toggle"`,
		`"always"`,
		`{"kind":"toggle","effort_map":{"minimal":"minimal","low":"low","medium":"medium","high":"high","xhigh":"xhigh"}}`,
	} {
		var patch ReasoningPatch
		if err := json.Unmarshal([]byte(raw), &patch); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		encoded, err := json.Marshal(patch)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var decoded ReasoningPatch
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("re-decode %s: %v", encoded, err)
		}
		if (decoded.Kind == nil) != (patch.Kind == nil) ||
			(decoded.Kind != nil && *decoded.Kind != *patch.Kind) ||
			!maps.Equal(nonNilMap(decoded.EffortMap), nonNilMap(patch.EffortMap)) {
			t.Fatalf("round trip %s = %#v, want %#v", raw, decoded, patch)
		}
	}
}

func nonNilMap(m *map[ReasoningEffort]string) map[ReasoningEffort]string {
	if m == nil {
		return map[ReasoningEffort]string{}
	}
	return *m
}
