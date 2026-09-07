package bytedance

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

// TestEmbedDimensionsGate locks the capability-driven gate: models without
// the published capability reject the dimensions field, and models that
// publish it compile it through.
func TestEmbedDimensionsGate(t *testing.T) {
	fixed := catalogEntry{
		kind:         kindEmbed,
		capabilities: inference.ModelCapabilities{}.WithInputs(message.PartText),
	}
	compiled, err := compileEmbed("fixed", fixed)(
		context.Background(),
		conformanceModel("fixed"),
		embedDimensionRequest(),
	)
	if err == nil {
		t.Fatal("fixed-size model unexpectedly accepted custom dimensions")
	}
	if !compiled.Report.Rejects(inference.FieldEmbedDimensions) {
		t.Fatal("dimensions request was not rejected on the dimensions field")
	}

	compiled, err = compileEmbed(
		"doubao-embedding-large",
		catalog["doubao-embedding-large"],
	)(
		context.Background(),
		conformanceModel("doubao-embedding-large"),
		embedDimensionRequest(),
	)
	if err != nil {
		t.Fatalf("dimensions request rejected on a capable model: %v", err)
	}
	if compiled.Wire.dimensions == nil || *compiled.Wire.dimensions != 512 {
		t.Fatalf("wire dimensions = %v, want 512", compiled.Wire.dimensions)
	}
}

// TestCustomEmbedDimensionsRequiresEmbedKind guards the capability leaf's
// family contract on the spec side.
func TestCustomEmbedDimensionsRequiresEmbedKind(t *testing.T) {
	if _, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "m",
			"kind": "generate",
			"capabilities": {"outputs": ["text"], "custom_embed_dimensions": true}
		}]
	}`)); err == nil {
		t.Fatal("custom_embed_dimensions on a generate model unexpectedly accepted")
	}
}
