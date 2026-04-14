package knowledge

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/workspace"
)

type mockEmbedder struct {
	dim int
}

func (m *mockEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	vec := make([]float32, m.dim)
	for j := range vec {
		vec[j] = float32(len(text)%10) * 0.1
	}
	return vec, nil
}

func (m *mockEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	vecs := make([][]float32, len(texts))
	for i := range texts {
		vec := make([]float32, m.dim)
		for j := range vec {
			vec[j] = float32(len(texts[i])%10) * 0.1
		}
		vecs[i] = vec
	}
	return vecs, nil
}

func TestFSStore_WithEmbedder_HybridSearch(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	emb := &mockEmbedder{dim: 8}
	store := NewFSStore(ws, WithEmbedder(emb), WithChunkConfig(ChunkConfig{ChunkSize: 200, ChunkOverlap: 20}))
	ctx := context.Background()

	_ = store.AddDocument(ctx, "ds1", "go.md", "Go is a statically typed compiled programming language")
	_ = store.AddDocument(ctx, "ds1", "py.md", "Python is a high-level interpreted scripting language")

	results, err := store.Search(ctx, "ds1", "programming language", SearchOptions{TopK: 5, Mode: ModeHybrid})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected hybrid search results")
	}
}

func TestFSStore_WithEmbedder_SemanticSearch(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	emb := &mockEmbedder{dim: 8}
	store := NewFSStore(ws, WithEmbedder(emb), WithChunkConfig(ChunkConfig{ChunkSize: 200, ChunkOverlap: 20}))
	ctx := context.Background()

	_ = store.AddDocument(ctx, "ds1", "go.md", "Go is a statically typed compiled programming language")

	results, err := store.Search(ctx, "ds1", "Go programming", SearchOptions{TopK: 5, Mode: ModeSemantic})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected semantic search results")
	}
}

func TestFSStore_NoEmbedder_HybridDegradesToBM25(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := NewFSStore(ws, WithChunkConfig(ChunkConfig{ChunkSize: 200, ChunkOverlap: 20}))
	ctx := context.Background()

	_ = store.AddDocument(ctx, "ds1", "go.md", "Go is a statically typed compiled programming language")

	results, err := store.Search(ctx, "ds1", "Go programming", SearchOptions{TopK: 5, Mode: ModeHybrid})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected results (degraded to BM25)")
	}
}
