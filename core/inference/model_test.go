package inference_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
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

func TestModelCapabilitiesValidate(t *testing.T) {
	capabilities := inference.ModelCapabilities{
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
		if err := (inference.ModelCapabilities{Outputs: outputs}).Validate(); err == nil {
			t.Fatalf("outputs %v unexpectedly accepted", outputs)
		}
	}

	for _, inputs := range [][]message.PartKind{
		{message.PartText, message.PartText},
		{"unknown_kind"},
	} {
		if err := (inference.ModelCapabilities{Inputs: inputs}).Validate(); err == nil {
			t.Fatalf("inputs %v unexpectedly accepted", inputs)
		}
	}

	for _, reasoning := range []inference.ReasoningKind{
		inference.ReasoningAlways,
		inference.ReasoningToggle,
	} {
		if err := (inference.ModelCapabilities{
			Reasoning: reasoning,
		}).Validate(); err != nil {
			t.Fatalf("reasoning %q: %v", reasoning, err)
		}
	}
	if err := (inference.ModelCapabilities{
		Reasoning: "sometimes",
	}).Validate(); err == nil {
		t.Fatal("unknown reasoning kind unexpectedly accepted")
	}
}

func TestReasoningKindValidate(t *testing.T) {
	for _, kind := range []inference.ReasoningKind{
		inference.ReasoningNone,
		inference.ReasoningAlways,
		inference.ReasoningToggle,
	} {
		if err := kind.Validate(); err != nil {
			t.Fatalf("kind %q: %v", kind, err)
		}
	}
	if err := (inference.ReasoningKind("sometimes")).Validate(); err == nil {
		t.Fatal("unknown reasoning kind unexpectedly accepted")
	}
}

func TestModelCapabilitiesCloneDoesNotShareSlices(t *testing.T) {
	original := inference.ModelCapabilities{
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
	capabilities := inference.ModelCapabilities{}.
		WithInputs(message.PartText, message.PartImage).
		WithInputs(message.PartData).
		WithOutputs(message.PartText).
		WithReasoning(inference.ReasoningAlways).
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
	if capabilities.Reasoning != inference.ReasoningAlways ||
		!capabilities.HostedWebSearch {
		t.Fatalf("capabilities = %+v", capabilities)
	}
	if err := capabilities.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestModelCapabilitiesBuildersDoNotAlias(t *testing.T) {
	base := inference.ModelCapabilities{}.WithInputs(message.PartText)
	extended := base.WithInputs(message.PartImage)
	if len(base.Inputs) != 1 || base.Inputs[0] != message.PartText {
		t.Fatalf("base inputs mutated by builder: %v", base.Inputs)
	}
	_ = extended
}

func TestModelDescriptorClonePreservesCapabilities(t *testing.T) {
	original := inference.ModelDescriptor{
		ID:         inference.ModelID{Provider: "openai", Name: "gpt-x"},
		Operations: []inference.Operation{inference.OperationGenerate},
		Capabilities: inference.ModelCapabilities{
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
	descriptor := inference.ModelDescriptor{
		ID:         inference.ModelID{Provider: "openai", Name: "gpt-x"},
		Operations: []inference.Operation{inference.OperationGenerate},
		Capabilities: inference.ModelCapabilities{
			Outputs: []message.PartKind{message.PartToolCall},
		},
	}
	if err := descriptor.Validate(); err == nil {
		t.Fatal("tool_call output unexpectedly accepted")
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
