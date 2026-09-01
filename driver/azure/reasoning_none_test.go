package azure

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

func TestToggleFalseCompilesToEffortNone(t *testing.T) {
	entry := catalogEntry{
		kind: kindGenerate,
		capabilities: inference.ModelCapabilities{
			Outputs:   []message.PartKind{message.PartText},
			Reasoning: inference.ReasoningToggle,
		},
		effortNone: true,
	}
	compile := compileGenerate("deploy-none", entry)
	request := conformanceTextRequest()
	disabled := false
	request.Input.Content.Intent.Text.ReasoningEnabled = &disabled

	compiled, err := compile(
		context.Background(),
		conformanceModel("deploy-none"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if compiled.Wire.reasoning != "none" {
		t.Fatalf("wire reasoning = %q, want none", compiled.Wire.reasoning)
	}
	if compiled.Report.Rejects(inference.FieldGenerateIntentReasoningEnabled) {
		t.Fatal("disable request unexpectedly rejected on an effort_none deployment")
	}
}

func TestToggleFalseRejectsWithoutEffortNone(t *testing.T) {
	entry := catalogEntry{
		kind: kindGenerate,
		capabilities: inference.ModelCapabilities{
			Outputs:   []message.PartKind{message.PartText},
			Reasoning: inference.ReasoningToggle,
		},
	}
	compile := compileGenerate("deploy-legacy", entry)
	request := conformanceTextRequest()
	disabled := false
	request.Input.Content.Intent.Text.ReasoningEnabled = &disabled

	compiled, err := compile(
		context.Background(),
		conformanceModel("deploy-legacy"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err == nil {
		t.Fatal("disable request unexpectedly accepted without effort_none")
	}
	if !compiled.Report.Rejects(inference.FieldGenerateIntentReasoningEnabled) {
		t.Fatal("disable request was not rejected on the reasoning_enabled field")
	}
}
