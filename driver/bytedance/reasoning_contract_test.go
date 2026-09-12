package bytedance

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/inference/model"
)

// TestRedeclaredBuiltinKeepsReasoning locks the delta overlay contract:
// redeclaring doubao-seed-2-1-pro to tweak one channel must keep the
// built-in reasoning kind and effort map, so a published toggle still
// compiles reasoning_enabled=false.
func TestRedeclaredBuiltinKeepsReasoning(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "doubao-seed-2-1-pro",
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
	entry := models["doubao-seed-2-1-pro"]
	if entry.capabilities.Reasoning.Kind != model.ReasoningToggle {
		t.Fatalf("redeclaration must inherit reasoning toggle, got %q",
			entry.capabilities.Reasoning.Kind)
	}
	if len(entry.capabilities.Reasoning.EffortMap) == 0 {
		t.Fatal("redeclaration must inherit the built-in effort map")
	}
	compiled, err := compileGenerate("doubao-seed-2-1-pro", entry)(
		context.Background(),
		conformanceModel("doubao-seed-2-1-pro"),
		inferencetest.ReasoningOffProbe(),
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("toggle model rejected reasoning off: %v", err)
	}
	if compiled.Report.Rejects(inference.FieldGenerateIntentReasoningEnabled) {
		t.Fatal("toggle model rejected on the reasoning_enabled field")
	}
}

// TestRedeclaredBuiltinKeepsControlFlags locks flag inheritance for embed
// and video entries (dimensions, max resolution) on same-kind redeclare.
func TestRedeclaredBuiltinKeepsControlFlags(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(`{
		"models": [
			{"name": "doubao-embedding-large", "kind": "embed"},
			{"name": "doubao-seedance-2-5", "kind": "video"}
		]
	}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	embed := models["doubao-embedding-large"]
	in, _ := embed.limits.Values()
	if !embed.capabilities.CustomEmbedDimensions ||
		in != 4_095 {
		t.Fatalf("redeclared embed lost control facts: dims=%v in=%d",
			embed.capabilities.CustomEmbedDimensions, in)
	}
	video := models["doubao-seedance-2-5"]
	builtin := catalog["doubao-seedance-2-5"]
	if video.maxResolution != builtin.maxResolution {
		t.Fatalf("redeclared video max_resolution = %q, want built-in %q",
			video.maxResolution, builtin.maxResolution)
	}
	if video.video != builtin.video {
		t.Fatal("redeclared video lost the built-in parameter matrix")
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
		if entry.kind != kindGenerate ||
			entry.capabilities.Reasoning.Kind != model.ReasoningToggle {
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
