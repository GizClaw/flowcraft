package model

import (
	"encoding/json"
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
