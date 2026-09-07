package minimax

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/message"
)

// TestRedeclaredBuiltinKeepsReasoning locks the delta overlay contract:
// redeclaring MiniMax-M3 to tweak one channel must keep the built-in
// reasoning toggle, so a published toggle still compiles
// reasoning_enabled=false.
func TestRedeclaredBuiltinKeepsReasoning(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "MiniMax-M3",
			"kind": "generate",
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
	entry := models["MiniMax-M3"]
	if entry.capabilities.Reasoning.Kind != inference.ReasoningToggle {
		t.Fatalf("redeclaration must inherit reasoning toggle, got %q",
			entry.capabilities.Reasoning.Kind)
	}
	compiled, err := compileGenerate("MiniMax-M3", entry)(
		context.Background(),
		conformanceModel("MiniMax-M3"),
		inferencetest.ReasoningOffProbe(),
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("toggle model rejected reasoning off: %v", err)
	}
	if compiled.Wire.thinking == nil || *compiled.Wire.thinking {
		t.Fatalf("wire thinking = %v, want disabled", compiled.Wire.thinking)
	}
	if compiled.Report.Rejects(inference.FieldGenerateIntentReasoningEnabled) {
		t.Fatal("toggle model rejected on the reasoning_enabled field")
	}
}

// TestPublishedToggleCompilesReasoningOff is the generic conformance
// contract for every built-in toggle model.
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
		if entry.kind != kindGenerate ||
			entry.capabilities.Reasoning.Kind != inference.ReasoningToggle {
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

// TestCrossKindRedeclarationAppliesDeclaredCapabilities guards the
// cross-kind overlay path: a spec entry that redeclares a built-in name
// under another kind replaces the entry with the declared capability leaves
// over the conservative zero base (it neither inherits the other family's
// capabilities nor drops the declaration).
func TestCrossKindRedeclarationAppliesDeclaredCapabilities(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "MiniMax-M3",
			"kind": "image",
			"capabilities": {"outputs": ["image"]}
		}]
	}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	entry := models["MiniMax-M3"]
	if entry.kind != kindImage {
		t.Fatalf("kind = %q, want image", entry.kind)
	}
	if len(entry.capabilities.Outputs) != 1 ||
		entry.capabilities.Outputs[0] != message.PartImage {
		t.Fatalf("outputs = %v, want declared image", entry.capabilities.Outputs)
	}
}

// TestCustomEmbedDimensionsUnsupported guards the capability leaf on a
// provider with no embed family.
func TestCustomEmbedDimensionsUnsupported(t *testing.T) {
	if _, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "m",
			"kind": "generate",
			"capabilities": {"outputs": ["text"], "custom_embed_dimensions": true}
		}]
	}`)); err == nil {
		t.Fatal("custom_embed_dimensions on minimax unexpectedly accepted")
	}
}
