package azure

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

func embedDimensionRequest() inference.EmbedRequest {
	dimensions := 512
	return inference.EmbedRequest{
		Items: []inference.EmbedItem{{
			Content: message.Content{Parts: []message.Part{
				message.TextPart{Text: "hi"},
			}},
		}},
		Dimensions: &dimensions,
	}
}

func embedSpec(leaf *bool) ModelSpec {
	inputs := []message.PartKind{message.PartText}
	return ModelSpec{
		Name: "embed-deploy",
		Kind: "embed",
		Capabilities: &inference.CapabilitiesPatch{
			Inputs:                &inputs,
			CustomEmbedDimensions: leaf,
		},
	}
}

// TestEmbedDimensionsGate locks the capability-driven gate: deployments
// without the published capability reject the dimensions field, and ones
// that publish it compile it through.
func TestEmbedDimensionsGate(t *testing.T) {
	compiled, err := compileEmbed("embed-deploy", entryFor(embedSpec(nil)))(
		context.Background(),
		conformanceModel("embed-deploy"),
		embedDimensionRequest(),
	)
	if err == nil {
		t.Fatal("fixed-size deployment unexpectedly accepted custom dimensions")
	}
	if !compiled.Report.Rejects(inference.FieldEmbedDimensions) {
		t.Fatal("dimensions request was not rejected on the dimensions field")
	}

	enabled := true
	compiled, err = compileEmbed("embed-deploy", entryFor(embedSpec(&enabled)))(
		context.Background(),
		conformanceModel("embed-deploy"),
		embedDimensionRequest(),
	)
	if err != nil {
		t.Fatalf("dimensions request rejected on a capable deployment: %v", err)
	}
	if compiled.Wire.dimensions == nil || *compiled.Wire.dimensions != 512 {
		t.Fatalf("wire dimensions = %v, want 512", compiled.Wire.dimensions)
	}
}

// TestCustomEmbedDimensionsRequiresEmbedKind guards the capability leaf's
// family contract on the spec side.
func TestCustomEmbedDimensionsRequiresEmbedKind(t *testing.T) {
	if _, err := decodeSpec(context.Background(), []byte(`{
		"endpoint": "https://example.openai.azure.com",
		"models": [{
			"name": "m",
			"kind": "generate",
			"capabilities": {"outputs": ["text"], "custom_embed_dimensions": true}
		}]
	}`)); err == nil {
		t.Fatal("custom_embed_dimensions on a generate deployment unexpectedly accepted")
	}
}

// TestCustomEmbedDimensionsFalseOnGenerateAccepted guards the leaf
// contract: an explicit false is the conservative declaration and must be
// a harmless no-op on deployments that never embed.
func TestCustomEmbedDimensionsFalseOnGenerateAccepted(t *testing.T) {
	if _, err := decodeSpec(context.Background(), []byte(`{
		"endpoint": "https://example.openai.azure.com",
		"models": [{
			"name": "m",
			"kind": "generate",
			"capabilities": {"outputs": ["text"], "custom_embed_dimensions": false}
		}]
	}`)); err != nil {
		t.Fatalf("explicit false on a generate deployment rejected: %v", err)
	}
}
