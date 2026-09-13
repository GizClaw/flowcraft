package anthropic

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/inference/model"
)

// TestDeclaredToggleKeepsItsEffortMap locks the declaration contract: the
// capabilities block is the whole fact, so a toggle that names an effort map
// keeps it, and the published toggle compiles reasoning_enabled=false.
func TestDeclaredToggleKeepsItsEffortMap(t *testing.T) {
	spec := decodeSpecWithModels(t, "", "claude-sonnet-5")
	models, err := resolveModelsForTest(t, spec)
	if err != nil {
		t.Fatalf("resolveModels: %v", err)
	}
	entry := models["claude-sonnet-5"]
	if entry.spec.Capabilities.Reasoning.Kind != model.ReasoningToggle {
		t.Fatalf("declaration must state reasoning toggle, got %q",
			entry.spec.Capabilities.Reasoning.Kind)
	}
	if len(entry.spec.Capabilities.Reasoning.EffortMap) == 0 {
		t.Fatal("declaration must state the effort map")
	}
	compiled, err := compileGenerateFor("claude-sonnet-5", entry)(
		context.Background(),
		conformanceModel("claude-sonnet-5"),
		inferencetest.ReasoningOffProbe(),
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("toggle model rejected reasoning off: %v", err)
	}
	if compiled.Wire.Thinking.OfDisabled == nil {
		t.Fatalf("wire thinking = %+v, want disabled", compiled.Wire.Thinking)
	}
	if compiled.Report.Rejects(inference.FieldGenerateIntentReasoningEnabled) {
		t.Fatal("toggle model rejected on the reasoning_enabled field")
	}
}

// TestDeclaredWithoutReasoningRejectsTheSwitch locks the other side: a model
// that declares no reasoning control has no switch to compile, so the request
// is rejected rather than silently accepted.
func TestDeclaredWithoutReasoningRejectsTheSwitch(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "claude-plain",
			"capabilities": {"inputs": ["text"], "outputs": ["text"]}
		}]
	}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := resolveModelsForTest(t, spec)
	if err != nil {
		t.Fatalf("resolveModels: %v", err)
	}
	entry := models["claude-plain"]
	compiled, err := compileGenerateFor("claude-plain", entry)(
		context.Background(),
		conformanceModel("claude-plain"),
		inferencetest.ReasoningOffProbe(),
		inference.GenerateExecutionUnary,
	)
	if err == nil {
		t.Fatal("model without a reasoning channel accepted the switch")
	}
	if !compiled.Report.Rejects(inference.FieldGenerateIntentReasoningEnabled) {
		t.Fatalf("decisions = %+v, want a reasoning_enabled rejection",
			compiled.Report.Decisions)
	}
}

// TestPublishedToggleCompilesReasoningOff is the generic conformance
// contract: every declared model that publishes toggle must compile the
// shared reasoning-off probe.
func TestPublishedToggleCompilesReasoningOff(t *testing.T) {
	spec := decodeSpecWithModels(t, "", fixtureNames...)
	models, err := resolveModelsForTest(t, spec)
	if err != nil {
		t.Fatalf("resolveModels: %v", err)
	}
	checked := 0
	for name, entry := range models {
		if entry.spec.Capabilities.Reasoning.Kind != model.ReasoningToggle {
			continue
		}
		checked++
		compiled, err := compileGenerateFor(name, entry)(
			context.Background(),
			conformanceModel(name),
			inferencetest.ReasoningOffProbe(),
			inference.GenerateExecutionUnary,
		)
		if err != nil {
			t.Fatalf("toggle model %q rejected reasoning off: %v", name, err)
		}
		if compiled.Report.Rejects(inference.FieldGenerateIntentReasoningEnabled) {
			t.Fatalf("toggle model %q rejected on the reasoning_enabled field", name)
		}
	}
	if checked == 0 {
		t.Fatal("no toggle model exercised")
	}
}

// TestCustomEmbedDimensionsUnsupported guards the capability leaf on a
// provider that serves text generation only.
func TestCustomEmbedDimensionsUnsupported(t *testing.T) {
	if _, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "m",
			"capabilities": {"outputs": ["text"], "custom_embed_dimensions": true}
		}]
	}`)); err == nil {
		t.Fatal("custom_embed_dimensions on anthropic unexpectedly accepted")
	}
}
