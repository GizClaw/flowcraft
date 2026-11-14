package openai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

func embedRequest(docs ...string) inference.EmbedRequest {
	input := make([]message.Content, 0, len(docs))
	for _, doc := range docs {
		input = append(input, message.NewTextContent(doc))
	}
	return inference.EmbedRequest{
		Model: model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: "text-embedding-3-small"}},
		Input: input,
	}
}

func embedProvider(t *testing.T, body string) inference.Embedder {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)

	provider, err := New(Settings{API: APIChat, APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	embedder := provider.Embedder
	if embedder == nil {
		t.Fatal("provider does not expose Embedder")
	}
	return embedder
}

// The provider's indices decide placement: an answer delivered out of order must
// not attribute one document's vector to another.
func TestEmbedPlacesVectorsByIndex(t *testing.T) {
	embedder := embedProvider(t, `{"data":[
		{"embedding":[0.3,0.4],"index":1},
		{"embedding":[0.1,0.2],"index":0}],
		"usage":{"prompt_tokens":2,"total_tokens":2}}`)

	response, err := embedder.Embed(context.Background(), embedRequest("a", "b"))
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(response.Vectors) != 2 {
		t.Fatalf("vectors = %d, want 2", len(response.Vectors))
	}
	if response.Vectors[0][0] != 0.1 || response.Vectors[1][0] != 0.3 {
		t.Errorf("vectors placed by arrival order, not by index: %v", response.Vectors)
	}
	if response.Usage.InputTokens != 2 {
		t.Errorf("input tokens = %d, want 2", response.Usage.InputTokens)
	}
}

// A width the provider ignored is rejected by the contract, not stored.
func TestEmbedRejectsIgnoredDimensions(t *testing.T) {
	embedder := embedProvider(t, `{"data":[{"embedding":[0.1,0.2],"index":0}],"usage":{"prompt_tokens":1,"total_tokens":1}}`)

	request := embedRequest("a")
	request.Dimensions = 4
	if _, err := embedder.Embed(context.Background(), request); !inference.IsKind(err, inference.InvalidProviderResponse) {
		t.Errorf("kind = %v, want InvalidProviderResponse", err)
	}
}

// Non-text documents are refused before any provider I/O: this endpoint embeds
// text, and silently dropping an image part would answer a different question.
func TestEmbedRejectsNonTextDocuments(t *testing.T) {
	embedder := embedProvider(t, `{"data":[],"usage":{}}`)
	request := embedRequest("a")
	source, err := media.NewImageBytes([]byte("x"), "image/png")
	if err != nil {
		t.Skipf("image source unavailable: %v", err)
	}
	request.Input = []message.Content{{Parts: []message.Part{message.ImagePart{Source: source}}}}
	if _, err := embedder.Embed(context.Background(), request); !inference.IsKind(err, inference.UnsupportedFeature) {
		t.Errorf("kind = %v, want UnsupportedFeature", err)
	}
}
