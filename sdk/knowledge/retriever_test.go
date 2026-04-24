package knowledge

import (
	"context"
	"errors"
	"testing"
)

// stubChunkRepo records the queries it sees and returns canned candidates.
type stubChunkRepo struct {
	calls []ChunkQuery
	hits  []Candidate
	err   error
}

func (r *stubChunkRepo) Replace(context.Context, string, string, []DerivedChunk) error {
	return nil
}
func (r *stubChunkRepo) DeleteByDoc(context.Context, string, string) error  { return nil }
func (r *stubChunkRepo) DeleteByDataset(context.Context, string) error      { return nil }
func (r *stubChunkRepo) Search(_ context.Context, q ChunkQuery) ([]Candidate, error) {
	r.calls = append(r.calls, q)
	if r.err != nil {
		return nil, r.err
	}
	return r.hits, nil
}

type stubLayerRepo struct {
	calls []LayerQuery
	hits  []Candidate
	err   error
}

func (r *stubLayerRepo) Put(context.Context, DerivedLayer) error { return nil }
func (r *stubLayerRepo) Get(context.Context, string, string, Layer) (*DerivedLayer, error) {
	return nil, nil
}
func (r *stubLayerRepo) DeleteByDoc(context.Context, string, string) error { return nil }
func (r *stubLayerRepo) DeleteByDataset(context.Context, string) error     { return nil }
func (r *stubLayerRepo) Search(_ context.Context, q LayerQuery) ([]Candidate, error) {
	r.calls = append(r.calls, q)
	if r.err != nil {
		return nil, r.err
	}
	return r.hits, nil
}

// fixedEmbedder returns the same vector regardless of input.
type fixedEmbedder struct {
	vec []float32
	err error
}

func (e *fixedEmbedder) Embed(context.Context, string) ([]float32, error) {
	return e.vec, e.err
}
func (e *fixedEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = e.vec
	}
	return out, e.err
}

func TestBM25Retriever_SkipsNonDetailLayer(t *testing.T) {
	repo := &stubChunkRepo{}
	r := NewBM25Retriever(repo)
	out, err := r.Recall(context.Background(), Query{Layer: LayerAbstract, Text: "x"})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if out != nil {
		t.Fatalf("expected nil for layer query")
	}
	if len(repo.calls) != 0 {
		t.Fatalf("repo should not have been called")
	}
}

func TestBM25Retriever_SkipsModeVector(t *testing.T) {
	repo := &stubChunkRepo{}
	r := NewBM25Retriever(repo)
	if _, err := r.Recall(context.Background(), Query{Layer: LayerDetail, Mode: ModeVector, Text: "x"}); err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(repo.calls) != 0 {
		t.Fatalf("repo should not be hit for vector mode")
	}
}

func TestBM25Retriever_PushesDownDatasets(t *testing.T) {
	repo := &stubChunkRepo{}
	r := NewBM25Retriever(repo)
	q := Query{Layer: LayerDetail, Mode: ModeBM25, Text: "x", TopK: 5}.withDatasets([]string{"ds1", "ds2"})
	if _, err := r.Recall(context.Background(), q); err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(repo.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(repo.calls))
	}
	got := repo.calls[0]
	if len(got.DatasetIDs) != 2 || got.DatasetIDs[0] != "ds1" || got.DatasetIDs[1] != "ds2" {
		t.Fatalf("dataset ids = %+v", got.DatasetIDs)
	}
	if got.Mode != ModeBM25 || got.Text != "x" {
		t.Fatalf("query mismatch: %+v", got)
	}
}

func TestVectorRetriever_DisabledWithoutEmbedder(t *testing.T) {
	repo := &stubChunkRepo{}
	r := NewVectorRetriever(repo, nil)
	out, err := r.Recall(context.Background(), Query{Layer: LayerDetail, Mode: ModeVector, Text: "x"})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if out != nil {
		t.Fatalf("expected nil when embedder is nil")
	}
}

func TestVectorRetriever_EmbeddingErrorBubbles(t *testing.T) {
	repo := &stubChunkRepo{}
	emb := &fixedEmbedder{err: errors.New("boom")}
	r := NewVectorRetriever(repo, emb)
	q := Query{Layer: LayerDetail, Mode: ModeVector, Text: "x"}.withDatasets([]string{"ds"})
	if _, err := r.Recall(context.Background(), q); err == nil {
		t.Fatalf("expected error from embedder")
	}
}

func TestVectorRetriever_SendsVectorQuery(t *testing.T) {
	repo := &stubChunkRepo{}
	emb := &fixedEmbedder{vec: []float32{1, 2, 3}}
	r := NewVectorRetriever(repo, emb)
	q := Query{Layer: LayerDetail, Mode: ModeVector, Text: "x", TopK: 5}.withDatasets([]string{"ds"})
	if _, err := r.Recall(context.Background(), q); err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(repo.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(repo.calls))
	}
	got := repo.calls[0]
	if got.Mode != ModeVector || len(got.Vector) != 3 || got.Vector[0] != 1 {
		t.Fatalf("query mismatch: %+v", got)
	}
}

func TestLayerRetriever_OnlyForLayerQueries(t *testing.T) {
	repo := &stubLayerRepo{}
	r := NewLayerRetriever(repo, nil)
	out, err := r.Recall(context.Background(), Query{Layer: LayerDetail, Text: "x"})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if out != nil {
		t.Fatalf("expected nil for LayerDetail")
	}
	if len(repo.calls) != 0 {
		t.Fatalf("repo should not be called")
	}
}

func TestLayerRetriever_VectorLaneSkippedWithoutEmbedder(t *testing.T) {
	repo := &stubLayerRepo{}
	r := NewLayerRetriever(repo, nil)
	q := Query{Layer: LayerAbstract, Mode: ModeVector, Text: "x"}.withDatasets([]string{"ds"})
	if _, err := r.Recall(context.Background(), q); err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(repo.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(repo.calls))
	}
	if repo.calls[0].Mode != ModeBM25 {
		t.Fatalf("mode = %q, want bm25 (vector degraded)", repo.calls[0].Mode)
	}
}

func TestLayerRetriever_HybridUsesBoth(t *testing.T) {
	repo := &stubLayerRepo{}
	emb := &fixedEmbedder{vec: []float32{1, 2}}
	r := NewLayerRetriever(repo, emb)
	q := Query{Layer: LayerOverview, Mode: ModeHybrid, Text: "x"}.withDatasets([]string{"ds"})
	if _, err := r.Recall(context.Background(), q); err != nil {
		t.Fatalf("recall: %v", err)
	}
	if repo.calls[0].Mode != ModeHybrid {
		t.Fatalf("mode = %q, want hybrid", repo.calls[0].Mode)
	}
	if len(repo.calls[0].Vector) != 2 {
		t.Fatalf("vector not pushed: %+v", repo.calls[0].Vector)
	}
}

func TestRRFRanker_FusesMultipleSources(t *testing.T) {
	r := NewRRFRanker()
	out := r.Rank([]Candidate{
		{Source: "bm25", Hit: Hit{DocName: "a.md", ChunkIndex: 0, Score: 5}},
		{Source: "vector", Hit: Hit{DocName: "a.md", ChunkIndex: 0, Score: 0.9}},
		{Source: "bm25", Hit: Hit{DocName: "b.md", ChunkIndex: 0, Score: 2}},
	}, Query{TopK: 5})
	if len(out) != 2 {
		t.Fatalf("expected 2 unique hits, got %d", len(out))
	}
	if out[0].DocName != "a.md" {
		t.Fatalf("a.md should rank first, got %+v", out)
	}
}
