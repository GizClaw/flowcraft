package kimi

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/message"
)

// TestRedeclaredBuiltinKeepsReasoning locks the leaf-patch contract:
// redeclaring kimi-k2.6 without naming its reasoning leaf keeps the
// built-in reasoning toggle, and a published toggle still compiles
// reasoning_enabled=false.
func TestRedeclaredBuiltinKeepsReasoning(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "kimi-k2.6",
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
	entry := models["kimi-k2.6"]
	builtin := catalog["kimi-k2.6"]
	if entry.capabilities.Reasoning.Kind != inference.ReasoningToggle {
		t.Fatalf("redeclaration must keep reasoning toggle, got %q",
			entry.capabilities.Reasoning.Kind)
	}
	if !equalKinds(entry.capabilities.Inputs, builtin.capabilities.Inputs) {
		t.Fatalf("unstated inputs must be inherited: %v vs %v",
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

// TestRedeclaredBuiltinCanNarrow locks the leaf-replacement direction:
// a written inputs list narrows the built-in surface while unstated
// reasoning and limits are inherited.
func TestRedeclaredBuiltinCanNarrow(t *testing.T) {
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
	want := []message.PartKind{message.PartText}
	if !equalKinds(entry.capabilities.Inputs, want) {
		t.Fatalf("written inputs must replace the built-in list, got %v",
			entry.capabilities.Inputs)
	}
	if entry.capabilities.Reasoning.Kind != inference.ReasoningToggle {
		t.Fatalf("unstated reasoning must be inherited, got %q",
			entry.capabilities.Reasoning.Kind)
	}
	in, _ := entry.limits.Values()
	if in != 256_000 {
		t.Fatalf("unstated limits must be inherited, got %d", in)
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

	if _, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "m",
			"kind": "generate",
			"capabilities": {"outputs": ["text"], "custom_embed_dimensions": false}
		}]
	}`)); err != nil {
		t.Fatalf("explicit false on a generate model rejected: %v", err)
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
