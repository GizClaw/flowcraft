package openai

import (
	"context"
	"os"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
)

// TestEmbedCredentialed exercises embedding against the real endpoint. It skips
// without a key, so `go test ./...` stays hermetic while a credentialed lane
// proves the wire mapping — a fake server cannot catch a wrong model name, a
// rejected dimensions parameter, or a field the endpoint silently ignores.
func TestEmbedCredentialed(t *testing.T) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("OPENAI_API_KEY not set")
	}
	provider, err := New(Settings{API: APIChat, APIKey: key})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	embedder := provider.Embedder
	if embedder == nil {
		t.Fatal("provider does not expose Embedder")
	}

	response, err := embedder.Embed(context.Background(), inference.EmbedRequest{
		Model: model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: "text-embedding-3-small"}},
		Input: []message.Content{
			message.NewTextContent("the first document"),
			message.NewTextContent("the second document"),
		},
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(response.Vectors) != 2 || len(response.Vectors[0]) == 0 {
		t.Fatalf("vectors = %d, first width = %d", len(response.Vectors), len(response.Vectors[0]))
	}
	t.Logf("default width = %d", len(response.Vectors[0]))

	truncated, err := embedder.Embed(context.Background(), inference.EmbedRequest{
		Model:      model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: "text-embedding-3-small"}},
		Input:      []message.Content{message.NewTextContent("the first document")},
		Dimensions: 64,
	})
	if err != nil {
		t.Fatalf("Embed(dimensions): %v", err)
	}
	if got := len(truncated.Vectors[0]); got != 64 {
		t.Fatalf("dimensions = %d, want the requested 64 (the contract would reject a mismatch)", got)
	}
	t.Logf("requested width = %d", 64)
}
