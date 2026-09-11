package model

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"
)

func TestReasoningKindValidate(t *testing.T) {
	for _, kind := range []ReasoningKind{
		ReasoningNone,
		ReasoningAlways,
		ReasoningToggle,
	} {
		if err := kind.Validate(); err != nil {
			t.Fatalf("kind %q: %v", kind, err)
		}
	}
	if err := (ReasoningKind("sometimes")).Validate(); err == nil {
		t.Fatal("unknown reasoning kind unexpectedly accepted")
	}
}

func TestReasoningCapabilityJSONBackwardCompat(t *testing.T) {
	var legacy ReasoningCapability
	if err := json.Unmarshal(
		[]byte(`"toggle"`),
		&legacy,
	); err != nil {
		t.Fatalf("legacy string form: %v", err)
	}
	if legacy.Kind != ReasoningToggle ||
		len(legacy.EffortMap) != 0 {
		t.Fatalf("legacy decode = %+v", legacy)
	}
	legacyEncoded, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("legacy marshal: %v", err)
	}
	if string(legacyEncoded) != `"toggle"` {
		t.Fatalf("legacy marshal = %s, want \"toggle\"", legacyEncoded)
	}

	var object ReasoningCapability
	if err := json.Unmarshal([]byte(`{
		"kind": "always",
		"effort_map": {
			"minimal": "low",
			"low": "low",
			"medium": "high",
			"high": "high",
			"xhigh": "max"
		}
	}`), &object); err != nil {
		t.Fatalf("object form: %v", err)
	}
	if object.Kind != ReasoningAlways ||
		object.EffortMap[ReasoningXHigh] != "max" {
		t.Fatalf("object decode = %+v", object)
	}
	if err := object.Validate(); err != nil {
		t.Fatalf("object Validate: %v", err)
	}

	encoded, err := json.Marshal(ModelCapabilities{
		Reasoning: object,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"kind":"always"`) ||
		!strings.Contains(string(encoded), `"xhigh":"max"`) {
		t.Fatalf("marshal object = %s", encoded)
	}
	if encodedZero, err := json.Marshal(ModelCapabilities{}); err != nil ||
		strings.Contains(string(encodedZero), `"reasoning"`) {
		t.Fatalf("zero reasoning should be omitted, got %s", encodedZero)
	}
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
	if got.Kind != ReasoningNone || len(got.EffortMap) != 0 {
		t.Fatalf("none patch = %#v, want kind none with no effort map", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("removed reasoning must stay valid, got %v", err)
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
