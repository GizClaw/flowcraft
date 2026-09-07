package azure

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

func TestToggleFalseCompilesToWireNone(t *testing.T) {
	entry := catalogEntry{
		kind: kindGenerate,
		capabilities: inference.ModelCapabilities{
			Outputs:   []message.PartKind{message.PartText},
			Reasoning: inference.ReasoningCapability{Kind: inference.ReasoningToggle},
		},
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
		t.Fatal("disable request unexpectedly rejected on a toggle deployment")
	}
}

func TestLegacyToggleWithoutMapPassesEffortThrough(t *testing.T) {
	entry := catalogEntry{
		kind: kindGenerate,
		capabilities: inference.ModelCapabilities{
			Outputs:   []message.PartKind{message.PartText},
			Reasoning: inference.ReasoningCapability{Kind: inference.ReasoningToggle},
		},
	}
	compile := compileGenerate("deploy-legacy-effort", entry)
	request := conformanceTextRequest()
	request.Input.Content.Intent.Text.ReasoningEffort = inference.ReasoningHigh

	compiled, err := compile(
		context.Background(),
		conformanceModel("deploy-legacy-effort"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if compiled.Wire.reasoning != "high" {
		t.Fatalf("wire reasoning = %q, want high", compiled.Wire.reasoning)
	}
	if compiled.Report.Dropped(inference.FieldGenerateIntentReasoningEffort) {
		t.Fatal("legacy spec effort unexpectedly dropped")
	}
}
