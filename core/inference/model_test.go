package inference_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

func TestModelLimitsValidate(t *testing.T) {
	if err := (inference.ModelLimits{}).Validate(); err != nil {
		t.Fatalf("empty limits: %v", err)
	}
	positive := 128_000
	if err := (inference.ModelLimits{
		MaxInputTokens: &positive,
	}).Validate(); err != nil {
		t.Fatalf("positive limit: %v", err)
	}
	for _, value := range []int{0, -1} {
		limit := value
		if err := (inference.ModelLimits{
			MaxInputTokens: &limit,
		}).Validate(); err == nil {
			t.Fatalf("limit %d unexpectedly accepted", value)
		}
	}
}

func TestModelDescriptorValidateRejectsNonPositiveInputLimit(t *testing.T) {
	limit := 0
	descriptor := inference.ModelDescriptor{
		ID:         inference.ModelID{Provider: "openai", Name: "gpt-x"},
		Operations: []inference.Operation{inference.OperationGenerate},
		Limits:     inference.ModelLimits{MaxInputTokens: &limit},
	}
	if err := descriptor.Validate(); err == nil {
		t.Fatal("non-positive max input tokens unexpectedly accepted")
	}
}

func TestModelDescriptorClonePreservesLimits(t *testing.T) {
	limit := 200_000
	original := inference.ModelDescriptor{
		ID:         inference.ModelID{Provider: "openai", Name: "gpt-x"},
		Operations: []inference.Operation{inference.OperationGenerate},
		Limits: inference.ModelLimits{
			MaxInputTokens: &limit,
		},
	}
	clone := original.Clone()
	*clone.Limits.MaxInputTokens = 100
	if *original.Limits.MaxInputTokens != 200_000 {
		t.Fatalf(
			"clone shares max input tokens pointer: original = %d",
			*original.Limits.MaxInputTokens,
		)
	}
}

func TestModelDescriptorJSONLimits(t *testing.T) {
	descriptor := inference.ModelDescriptor{
		ID:         inference.ModelID{Provider: "openai", Name: "gpt-x"},
		Operations: []inference.Operation{inference.OperationGenerate},
	}
	encoded, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatalf("marshal empty limits: %v", err)
	}
	if got := string(encoded); strings.Contains(got, `"limits"`) {
		t.Fatalf("empty limits should be omitted, got %s", got)
	}

	limit := 128_000
	descriptor.Limits.MaxInputTokens = &limit
	encoded, err = json.Marshal(descriptor)
	if err != nil {
		t.Fatalf("marshal limits: %v", err)
	}
	var decoded struct {
		Limits inference.ModelLimits `json:"limits"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal limits: %v", err)
	}
	if decoded.Limits.MaxInputTokens == nil ||
		*decoded.Limits.MaxInputTokens != 128_000 {
		t.Fatalf("decoded limits = %+v, want max_input_tokens 128000", decoded.Limits)
	}
}
