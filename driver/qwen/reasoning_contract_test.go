package qwen

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/message"
)

// TestRedeclaredBuiltinKeepsReasoning locks Qwen's additive overlay
// contract: a spec declaration can widen a built-in's surface but never
// drop what the catalog already promises, so redeclaring qwen3.7-max keeps
// its reasoning toggle and a published toggle still compiles
// reasoning_enabled=false.
func TestRedeclaredBuiltinKeepsReasoning(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "qwen3.7-max",
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
	entry := models["qwen3.7-max"]
	builtin := catalog["qwen3.7-max"]
	if entry.capabilities.Reasoning.Kind != inference.ReasoningToggle {
		t.Fatalf("redeclaration must keep reasoning toggle, got %q",
			entry.capabilities.Reasoning.Kind)
	}
	if !equalKinds(entry.capabilities.Inputs, builtin.capabilities.Inputs) {
		t.Fatalf("redeclaration must not narrow built-in inputs: %v vs %v",
			entry.capabilities.Inputs, builtin.capabilities.Inputs)
	}
	if _, err := compileGenerate("qwen3.7-max", entry)(
		context.Background(),
		conformanceModel("qwen3.7-max"),
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

// TestCustomEmbedDimensionsNotConfigurable guards the capability leaf on
// Qwen specs: exact sizes come from the built-in whitelist, so a
// declaration cannot be honored and must fail at decode time.
func TestCustomEmbedDimensionsNotConfigurable(t *testing.T) {
	if _, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "text-embedding-v4",
			"kind": "embed",
			"capabilities": {
				"inputs": ["text"],
				"custom_embed_dimensions": true
			}
		}]
	}`)); err == nil {
		t.Fatal("custom_embed_dimensions on a qwen spec unexpectedly accepted")
	}
}

// TestEmbedWhitelistMatchesPublishedCapability guards the invariant that
// the published capability and the private size whitelist cannot drift.
func TestEmbedWhitelistMatchesPublishedCapability(t *testing.T) {
	base := catalogEntry{
		kind:         kindEmbed,
		capabilities: inference.ModelCapabilities{}.WithInputs(message.PartText),
	}
	if err := base.validate(); err != nil {
		t.Fatalf("consistent zero declaration rejected: %v", err)
	}
	claimWithoutSizes := base
	claimWithoutSizes.capabilities.CustomEmbedDimensions = true
	if err := claimWithoutSizes.validate(); err == nil {
		t.Fatal("capability without a whitelist unexpectedly accepted")
	}
	sizesWithoutClaim := base
	sizesWithoutClaim.embedDimensions = []int{64}
	if err := sizesWithoutClaim.validate(); err == nil {
		t.Fatal("whitelist without the capability unexpectedly accepted")
	}
}
