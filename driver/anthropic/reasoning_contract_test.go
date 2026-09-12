package anthropic

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/inference/model"
)

// TestRedeclaredBuiltinKeepsReasoning locks the delta overlay contract:
// redeclaring claude-sonnet-5 to tweak one channel must keep the built-in
// reasoning kind and effort map, so a published toggle still compiles
// reasoning_enabled=false.
func TestRedeclaredBuiltinKeepsReasoning(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "claude-sonnet-5",
			"capabilities": {"outputs": ["text"]}
		}]
	}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	entry := models["claude-sonnet-5"]
	if entry.capabilities.Reasoning.Kind != model.ReasoningToggle {
		t.Fatalf("redeclaration must inherit reasoning toggle, got %q",
			entry.capabilities.Reasoning.Kind)
	}
	if len(entry.capabilities.Reasoning.EffortMap) == 0 {
		t.Fatal("redeclaration must inherit the built-in effort map")
	}
	compiled, err := compileGenerate("claude-sonnet-5", entry)(
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

// TestRedeclaredBuiltinRemovesReasoning locks the removal contract: the
// legacy `reasoning: ""` leaf strips a built-in's reasoning kind and the
// effort map inherited with it, leaving a coherent none capability that
// still passes merged-catalog validation.
func TestRedeclaredBuiltinRemovesReasoning(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "claude-sonnet-5",
			"capabilities": {"reasoning": ""}
		}]
	}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	entry := models["claude-sonnet-5"]
	if entry.capabilities.Reasoning.Kind != model.ReasoningNone {
		t.Fatalf("reasoning kind = %q, want none", entry.capabilities.Reasoning.Kind)
	}
	if len(entry.capabilities.Reasoning.EffortMap) != 0 {
		t.Fatalf("removed reasoning must drop the inherited effort map, got %v",
			entry.capabilities.Reasoning.EffortMap)
	}
}

// TestPublishedToggleCompilesReasoningOff is the generic conformance
// contract for every built-in model that publishes toggle.
func TestPublishedToggleCompilesReasoningOff(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	checked := 0
	for name, entry := range models {
		if entry.capabilities.Reasoning.Kind != model.ReasoningToggle {
			continue
		}
		checked++
		compiled, err := compileGenerate(name, entry)(
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
