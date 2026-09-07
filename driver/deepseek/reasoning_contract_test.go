package deepseek

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
)

// TestRedeclaredBuiltinKeepsFacts locks the delta overlay contract:
// redeclaring deepseek-v4-flash to tweak one channel must keep the built-in
// reasoning kind and effort map, so the published toggle still compiles
// reasoning_enabled=false on both surfaces.
func TestRedeclaredBuiltinKeepsFacts(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "deepseek-v4-flash",
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
	entry := models["deepseek-v4-flash"]
	if entry.capabilities.Reasoning.Kind != inference.ReasoningToggle {
		t.Fatalf("redeclaration must inherit reasoning toggle, got %q",
			entry.capabilities.Reasoning.Kind)
	}
	if len(entry.capabilities.Reasoning.EffortMap) == 0 {
		t.Fatal("redeclaration must inherit the built-in effort map")
	}
	in, _ := entry.limits.Values()
	if in != 1_000_000 {
		t.Fatalf("redeclaration must inherit the built-in input limit, got %d", in)
	}
	if compiled, err := compileChatGenerate("deepseek-v4-flash", entry)(
		context.Background(),
		conformanceModel("deepseek-v4-flash"),
		inferencetest.ReasoningOffProbe(),
		inference.GenerateExecutionUnary,
	); err != nil {
		t.Fatalf("chat toggle rejected reasoning off: %v", err)
	} else if compiled.Report.Rejects(
		inference.FieldGenerateIntentReasoningEnabled,
	) {
		t.Fatal("chat toggle rejected on the reasoning_enabled field")
	}
	if compiled, err := compileResponsesGenerate("deepseek-v4-flash", entry)(
		context.Background(),
		conformanceModel("deepseek-v4-flash"),
		inferencetest.ReasoningOffProbe(),
		inference.GenerateExecutionUnary,
	); err != nil {
		t.Fatalf("responses toggle rejected reasoning off: %v", err)
	} else if compiled.Report.Rejects(
		inference.FieldGenerateIntentReasoningEnabled,
	) {
		t.Fatal("responses toggle rejected on the reasoning_enabled field")
	}
}

// TestPublishedToggleCompilesReasoningOff is the generic conformance
// contract for every built-in toggle model on both DeepSeek surfaces.
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
		if _, err := compileChatGenerate(name, entry)(
			context.Background(),
			conformanceModel(name),
			inferencetest.ReasoningOffProbe(),
			inference.GenerateExecutionUnary,
		); err != nil {
			t.Fatalf("chat toggle model %q rejected reasoning off: %v", name, err)
		}
		if _, err := compileResponsesGenerate(name, entry)(
			context.Background(),
			conformanceModel(name),
			inferencetest.ReasoningOffProbe(),
			inference.GenerateExecutionUnary,
		); err != nil {
			t.Fatalf("responses toggle model %q rejected reasoning off: %v", name, err)
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
		t.Fatal("custom_embed_dimensions on deepseek unexpectedly accepted")
	}
}

// TestCustomEmbedDimensionsFalseAccepted guards the leaf contract: deepseek
// serves text generation only, so an explicit false is the conservative
// declaration and must decode without error.
func TestCustomEmbedDimensionsFalseAccepted(t *testing.T) {
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
