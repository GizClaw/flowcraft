package qwen

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/message"
)

// TestRedeclaredBuiltinKeepsReasoning locks the leaf-patch contract:
// redeclaring qwen3.7-max without naming its reasoning leaf keeps the
// built-in reasoning toggle, and a published toggle still compiles
// reasoning_enabled=false.
func TestRedeclaredBuiltinKeepsReasoning(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "qwen3.7-max",
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
	entry := models["qwen3.7-max"]
	builtin := catalog["qwen3.7-max"]
	if entry.capabilities.Reasoning.Kind != inference.ReasoningToggle {
		t.Fatalf("redeclaration must keep reasoning toggle, got %q",
			entry.capabilities.Reasoning.Kind)
	}
	if !equalKinds(entry.capabilities.Inputs, builtin.capabilities.Inputs) {
		t.Fatalf("unstated inputs must be inherited: %v vs %v",
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

// TestRedeclaredBuiltinCanNarrow locks the leaf-replacement direction:
// a written inputs list narrows the built-in surface while unstated
// reasoning and limits are inherited.
func TestRedeclaredBuiltinCanNarrow(t *testing.T) {
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
	want := []message.PartKind{message.PartText}
	if !equalKinds(entry.capabilities.Inputs, want) {
		t.Fatalf("written inputs must replace the built-in list, got %v",
			entry.capabilities.Inputs)
	}
	if entry.capabilities.Reasoning.Kind != inference.ReasoningToggle {
		t.Fatalf("unstated reasoning must be inherited, got %q",
			entry.capabilities.Reasoning.Kind)
	}
	in, out := entry.limits.Values()
	if in != 991_808 || out != 131_072 {
		t.Fatalf("unstated limits must be inherited, got %d/%d", in, out)
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

// TestCustomEmbedDimensionsFamilyContract guards the capability leaf on
// Qwen specs: only whitelisted catalog entries carry the capability, so a
// declaration that tries to grant it to a non-embed model or to a custom
// embed model fails, while a same-kind redeclaration of a whitelisted
// entry stays consistent.
func TestCustomEmbedDimensionsFamilyContract(t *testing.T) {
	if _, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "m",
			"kind": "generate",
			"capabilities": {
				"outputs": ["text"],
				"custom_embed_dimensions": true
			}
		}]
	}`)); err == nil {
		t.Fatal("custom_embed_dimensions on a generate model unexpectedly accepted")
	}

	spec, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "custom-embed",
			"kind": "embed",
			"capabilities": {
				"inputs": ["text"],
				"custom_embed_dimensions": true
			}
		}]
	}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	if _, err := mergedCatalog(spec); err == nil {
		t.Fatal("custom embed model without a whitelist unexpectedly accepted")
	}

	spec, err = decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "text-embedding-v4",
			"kind": "embed",
			"capabilities": {
				"inputs": ["text"],
				"custom_embed_dimensions": true
			}
		}]
	}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		t.Fatalf("whitelisted redeclaration rejected: %v", err)
	}
	if !models["text-embedding-v4"].capabilities.CustomEmbedDimensions {
		t.Fatal("whitelisted redeclaration lost the custom dimensions capability")
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
