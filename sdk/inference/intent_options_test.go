package inference_test

import (
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
)

func TestToolChoiceValidates(t *testing.T) {
	cases := []struct {
		name   string
		ok     bool
		choice inference.ToolChoice
	}{
		{"auto", true, inference.ToolChoice{Kind: inference.ToolChoiceAuto}},
		{"none", true, inference.ToolChoice{Kind: inference.ToolChoiceNone}},
		{"required", true, inference.ToolChoice{Kind: inference.ToolChoiceRequired}},
		{"named with name", true, inference.ToolChoice{Kind: inference.ToolChoiceNamed, Name: "search"}},
		{"auto with name must be invalid", false, inference.ToolChoice{Kind: inference.ToolChoiceAuto, Name: "x"}},
		{"named without name must be invalid", false, inference.ToolChoice{Kind: inference.ToolChoiceNamed}},
		{"unknown kind must be invalid", false, inference.ToolChoice{Kind: inference.ToolChoiceKind("??")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.choice.Validate()
			if tc.ok && err != nil {
				t.Fatalf("expected valid, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("expected invalid, got nil")
			}
		})
	}
}

func TestResponseFormatValidates(t *testing.T) {
	cases := []struct {
		name   string
		ok     bool
		format inference.ResponseFormat
	}{
		{"text", true, inference.ResponseFormat{Kind: inference.ResponseText}},
		{"json object", true, inference.ResponseFormat{Kind: inference.ResponseJSONObject}},
		{
			"json schema",
			true,
			inference.ResponseFormat{
				Kind:   inference.ResponseJSONSchema,
				Name:   "answer",
				Schema: json.RawMessage(`{"type":"object"}`),
			},
		},
		{"text with schema must be invalid", false, inference.ResponseFormat{
			Kind:   inference.ResponseText,
			Schema: json.RawMessage(`{}`),
		}},
		{"json object with schema must be invalid", false, inference.ResponseFormat{
			Kind:   inference.ResponseJSONObject,
			Schema: json.RawMessage(`{}`),
		}},
		{"json schema without name must be invalid", false, inference.ResponseFormat{
			Kind:   inference.ResponseJSONSchema,
			Schema: json.RawMessage(`{"type":"object"}`),
		}},
		{"json schema without payload must be invalid", false, inference.ResponseFormat{
			Kind: inference.ResponseJSONSchema,
			Name: "answer",
		}},
		{"unknown kind must be invalid", false, inference.ResponseFormat{
			Kind: inference.ResponseFormatKind("??")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.format.Validate()
			if tc.ok && err != nil {
				t.Fatalf("expected valid, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("expected invalid, got nil")
			}
		})
	}
}

func TestFinishReasonValidates(t *testing.T) {
	for _, reason := range []inference.FinishReason{
		inference.FinishCompleted, inference.FinishMaxOutput, inference.FinishToolCalls,
		inference.FinishContentFilter, inference.FinishRefusal, inference.FinishPause,
		inference.FinishInvalidToolCall, inference.FinishContextLimit, inference.FinishOther,
	} {
		if err := reason.Validate(); err != nil {
			t.Fatalf("finish reason %q: %v", reason, err)
		}
	}
	if err := (inference.FinishReason("nope")).Validate(); err == nil {
		t.Fatal("unknown finish reason must be invalid")
	}
}

func TestReasoningEffortExposesKnownLevels(t *testing.T) {
	for _, effort := range []inference.ReasoningEffort{
		inference.ReasoningLow, inference.ReasoningMedium, inference.ReasoningHigh,
	} {
		if string(effort) == "" {
			t.Fatalf("empty reasoning effort constant")
		}
	}
}
