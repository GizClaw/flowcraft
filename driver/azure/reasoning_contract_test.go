package azure

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
)

func decodeDeployment(t *testing.T, raw string) Spec {
	t.Helper()
	spec, err := decodeSpec(context.Background(), []byte(`{
		"endpoint": "https://example.openai.azure.com",
		"models": [`+raw+`]
	}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	return spec
}

// TestSpecToggleCompilesOff locks the toggle contract for Azure: "toggle"
// on a deployment asserts the endpoint honors reasoning.effort="none", so
// no extra declaration is needed and the switch must compile.
func TestSpecToggleCompilesOff(t *testing.T) {
	spec := decodeDeployment(t, `{
		"name": "deploy-none",
		"kind": "generate",
		"capabilities": {"outputs": ["text"], "reasoning": {"kind": "toggle"}}
	}`)
	entry := entryFor(spec.Models[0])
	if err := entry.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	compiled, err := compileGenerate("deploy-none", entry)(
		context.Background(),
		conformanceModel("deploy-none"),
		inferencetest.ReasoningOffProbe(),
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("toggle deployment rejected reasoning off: %v", err)
	}
	if compiled.Wire.reasoning != "none" {
		t.Fatalf("wire reasoning = %q, want none", compiled.Wire.reasoning)
	}
	if compiled.Report.Rejects(inference.FieldGenerateIntentReasoningEnabled) {
		t.Fatal("toggle deployment rejected on the reasoning_enabled field")
	}
}

func TestSpecAlwaysRejectsOff(t *testing.T) {
	spec := decodeDeployment(t, `{
		"name": "deploy-always",
		"kind": "generate",
		"capabilities": {"outputs": ["text"], "reasoning": {"kind": "always"}}
	}`)
	entry := entryFor(spec.Models[0])
	if err := entry.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	compiled, err := compileGenerate("deploy-always", entry)(
		context.Background(),
		conformanceModel("deploy-always"),
		inferencetest.ReasoningOffProbe(),
		inference.GenerateExecutionUnary,
	)
	if err == nil {
		t.Fatal("always deployment unexpectedly compiled reasoning_enabled=false")
	}
	if !compiled.Report.Rejects(inference.FieldGenerateIntentReasoningEnabled) {
		t.Fatal("always deployment did not reject on the reasoning_enabled field")
	}
}
