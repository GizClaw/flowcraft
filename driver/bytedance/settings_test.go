package bytedance

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
)

func TestEmbedModelRequiresDimensions(t *testing.T) {
	embed := ModelSpec{
		Name: "doubao-embedding-vision",
		Kind: "embed",
		Capabilities: model.ModelCapabilities{
			Inputs:  []message.PartKind{message.PartText, message.PartImage},
			Outputs: nil,
		},
	}
	if err := embed.Validate(); err == nil {
		t.Fatal("embed model without embedding_dimensions was accepted")
	}
	embed.Capabilities.EmbeddingDimensions = 2048
	if err := embed.Validate(); err != nil {
		t.Fatalf("declared embed model rejected: %v", err)
	}
}
