package kimi

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/message"
)

// TestRedeclaredBuiltinKeepsReasoning locks Kimi's additive overlay
// contract: a spec declaration can widen a built-in's surface but never
// drop what the catalog already promises, so redeclaring kimi-k2.6 keeps
// its reasoning toggle and a published toggle still compiles
// reasoning_enabled=false.
func TestRedeclaredBuiltinKeepsReasoning(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "kimi-k2.6",
			"capabilities": {"inputs": ["text"]}
		}]
	}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	entry := models["kimi-k2.6"]
	builtin := catalog["kimi-k2.6"]
	if entry.capabilities.Reasoning.Kind != inference.ReasoningToggle {
		t.Fatalf("redeclaration must keep reasoning toggle, got %q",
			entry.capabilities.Reasoning.Kind)
	}
	if !equalKinds(entry.capabilities.Inputs, builtin.capabilities.Inputs) {
		t.Fatalf("redeclaration must not narrow built-in inputs: %v vs %v",
			entry.capabilities.Inputs, builtin.capabilities.Inputs)
	}
	if _, err := compileGenerate("kimi-k2.6", entry)(
		context.Background(),
		conformanceModel("kimi-k2.6"),
		inferencetest.ReasoningOffProbe(),
		inference.GenerateExecutionUnary,
	); err != nil {
		t.Fatalf("toggle model rejected reasoning off: %v", err)
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
		if entry.capabilities.Reasoning.Kind != inference.ReasoningToggle {
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
			"kind": "generate",
			"capabilities": {"outputs": ["text"], "custom_embed_dimensions": true}
		}]
	}`)); err == nil {
		t.Fatal("custom_embed_dimensions on kimi unexpectedly accepted")
	}
}

func equalKinds(a, b []message.PartKind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
