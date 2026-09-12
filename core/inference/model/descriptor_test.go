package model

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
)

func TestModelDescriptorValidateRejectsNonPositiveLimits(t *testing.T) {
	zero := 0
	for _, test := range []struct {
		name   string
		limits ModelLimits
	}{
		{
			name:   "input",
			limits: ModelLimits{MaxInputTokens: &zero},
		},
		{
			name:   "output",
			limits: ModelLimits{MaxOutputTokens: &zero},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			descriptor := ModelDescriptor{
				ID:         ModelID{Provider: "openai", Name: "gpt-x"},
				Operations: []Operation{OperationGenerate},
				Limits:     test.limits,
			}
			if err := descriptor.Validate(); err == nil {
				t.Fatalf("non-positive max %s tokens unexpectedly accepted", test.name)
			}
		})
	}
}

func TestModelDescriptorClonePreservesLimits(t *testing.T) {
	inputLimit := 200_000
	outputLimit := 16_384
	original := ModelDescriptor{
		ID:         ModelID{Provider: "openai", Name: "gpt-x"},
		Operations: []Operation{OperationGenerate},
		Limits: ModelLimits{
			MaxInputTokens:  &inputLimit,
			MaxOutputTokens: &outputLimit,
		},
	}
	clone := original.Clone()
	*clone.Limits.MaxInputTokens = 100
	*clone.Limits.MaxOutputTokens = 200
	if *original.Limits.MaxInputTokens != 200_000 {
		t.Fatalf(
			"clone shares max input tokens pointer: original = %d",
			*original.Limits.MaxInputTokens,
		)
	}
	if *original.Limits.MaxOutputTokens != 16_384 {
		t.Fatalf(
			"clone shares max output tokens pointer: original = %d",
			*original.Limits.MaxOutputTokens,
		)
	}
}

func TestModelDescriptorClonePreservesCapabilities(t *testing.T) {
	original := ModelDescriptor{
		ID:         ModelID{Provider: "openai", Name: "gpt-x"},
		Operations: []Operation{OperationGenerate},
		Capabilities: ModelCapabilities{
			Inputs:  []message.PartKind{message.PartText, message.PartImage},
			Outputs: []message.PartKind{message.PartText},
		},
	}
	clone := original.Clone()
	clone.Capabilities.Inputs[1] = message.PartAudio
	clone.Capabilities.Outputs[0] = message.PartImage
	if original.Capabilities.Inputs[1] != message.PartImage ||
		original.Capabilities.Outputs[0] != message.PartText {
		t.Fatalf(
			"descriptor clone shares capability slices: original = %+v",
			original.Capabilities,
		)
	}
}

func TestModelDescriptorValidateRejectsNonOutputModality(t *testing.T) {
	descriptor := ModelDescriptor{
		ID:         ModelID{Provider: "openai", Name: "gpt-x"},
		Operations: []Operation{OperationGenerate},
		Capabilities: ModelCapabilities{
			Outputs: []message.PartKind{message.PartToolCall},
		},
	}
	if err := descriptor.Validate(); err == nil {
		t.Fatal("tool_call output unexpectedly accepted")
	}
}

func TestModelDescriptorJSONLimits(t *testing.T) {
	descriptor := ModelDescriptor{
		ID:         ModelID{Provider: "openai", Name: "gpt-x"},
		Operations: []Operation{OperationGenerate},
	}
	encoded, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatalf("marshal empty limits: %v", err)
	}
	if got := string(encoded); strings.Contains(got, `"limits"`) {
		t.Fatalf("empty limits should be omitted, got %s", got)
	}

	inputLimit := 128_000
	outputLimit := 32_768
	descriptor.Limits.MaxInputTokens = &inputLimit
	descriptor.Limits.MaxOutputTokens = &outputLimit
	encoded, err = json.Marshal(descriptor)
	if err != nil {
		t.Fatalf("marshal limits: %v", err)
	}
	for _, tag := range []string{`"max_input_tokens"`, `"max_output_tokens"`} {
		if !strings.Contains(string(encoded), tag) {
			t.Fatalf("encoded limits %s missing %s", encoded, tag)
		}
	}
	var decoded struct {
		Limits ModelLimits `json:"limits"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal limits: %v", err)
	}
	if decoded.Limits.MaxInputTokens == nil ||
		*decoded.Limits.MaxInputTokens != inputLimit ||
		decoded.Limits.MaxOutputTokens == nil ||
		*decoded.Limits.MaxOutputTokens != outputLimit {
		t.Fatalf(
			"decoded limits = %+v, want max_input_tokens %d max_output_tokens %d",
			decoded.Limits, inputLimit, outputLimit,
		)
	}
}

// TestModelDescriptorWireFormat pins the deployment-spec contract: the JSON
// names, the omitzero/omitempty behavior, and round-trip stability. It
// replaced the extraction probe's cross-package comparison once this package
// became the single definition of the type.
func TestModelDescriptorWireFormat(t *testing.T) {
	const spec = `{
	  "id": {"provider": "kimi", "name": "kimi-k3"},
	  "label": "Kimi K3",
	  "operations": ["generate"],
	  "capabilities": {
	    "inputs": ["text", "image"],
	    "outputs": ["text"],
	    "reasoning": {
	      "kind": "toggle",
	      "effort_map": {
	        "minimal": "minimal",
	        "low": "low",
	        "medium": "medium",
	        "high": "high",
	        "xhigh": "max"
	      }
	    },
	    "hosted_web_search": true
	  },
	  "limits": {"max_input_tokens": 128000, "max_output_tokens": 8192},
	  "lifecycle": {
	    "status": "deprecated",
	    "retires_at": "2027-01-01T00:00:00Z",
	    "replacement": {"provider": "kimi", "name": "kimi-k4"},
	    "notes": "superseded"
	  }
	}`
	var descriptor ModelDescriptor
	if err := json.Unmarshal([]byte(spec), &descriptor); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := descriptor.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	encoded, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for _, key := range []string{
		`"id"`, `"label"`, `"operations"`, `"capabilities"`, `"inputs"`,
		`"outputs"`, `"reasoning"`, `"effort_map"`, `"hosted_web_search"`,
		`"limits"`, `"max_input_tokens"`, `"max_output_tokens"`,
		`"lifecycle"`, `"status"`, `"retires_at"`, `"replacement"`, `"notes"`,
	} {
		if !strings.Contains(string(encoded), key) {
			t.Errorf("encoded descriptor is missing %s: %s", key, encoded)
		}
	}
	var roundTripped ModelDescriptor
	if err := json.Unmarshal(encoded, &roundTripped); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if !reflect.DeepEqual(descriptor, roundTripped) {
		t.Fatalf("round trip changed the descriptor:\n%+v\n%+v", descriptor, roundTripped)
	}
}
