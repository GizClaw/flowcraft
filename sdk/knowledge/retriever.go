package knowledge

import (
	"context"
	"fmt"
)

// BM25Retriever queries ChunkRepo with Mode=ModeBM25. It is layered:
// queries whose Layer is not LayerDetail short-circuit to nil.
type BM25Retriever struct {
	Chunks ChunkRepo
}

// NewBM25Retriever constructs a BM25Retriever bound to a ChunkRepo.
func NewBM25Retriever(c ChunkRepo) *BM25Retriever { return &BM25Retriever{Chunks: c} }

// Name implements Retriever.
func (r *BM25Retriever) Name() string { return "bm25" }

// Recall implements Retriever. ModeVector queries are skipped because
// BM25 cannot satisfy them; ModeHybrid issues both lanes and the Ranker
// fuses the result.
func (r *BM25Retriever) Recall(ctx context.Context, q Query) ([]Candidate, error) {
	if r == nil || r.Chunks == nil || q.Layer != LayerDetail {
		return nil, nil
	}
	mode := ResolveMode(q.Mode)
	if mode == ModeVector {
		return nil, nil
	}
	return r.Chunks.Search(ctx, ChunkQuery{
		DatasetIDs: q.datasetIDs(),
		Text:       q.Text,
		Mode:       ModeBM25,
		TopK:       q.TopK,
	})
}

// VectorRetriever queries ChunkRepo with Mode=ModeVector. Embedder is
// invoked lazily; nil disables the retriever (Recall returns nil).
type VectorRetriever struct {
	Chunks   ChunkRepo
	Embedder Embedder
}

// NewVectorRetriever constructs a VectorRetriever; pass a nil embedder
// to disable the lane (Recall short-circuits).
func NewVectorRetriever(c ChunkRepo, e Embedder) *VectorRetriever {
	return &VectorRetriever{Chunks: c, Embedder: e}
}

// Name implements Retriever.
func (r *VectorRetriever) Name() string { return "vector" }

// Recall implements Retriever. ModeBM25 queries are skipped; ModeHybrid
// produces both lanes and the Ranker fuses them.
func (r *VectorRetriever) Recall(ctx context.Context, q Query) ([]Candidate, error) {
	if r == nil || r.Chunks == nil || r.Embedder == nil || q.Layer != LayerDetail {
		return nil, nil
	}
	mode := ResolveMode(q.Mode)
	if mode == ModeBM25 {
		return nil, nil
	}
	if q.Text == "" {
		return nil, nil
	}
	vec, err := r.Embedder.Embed(ctx, q.Text)
	if err != nil {
		return nil, fmt.Errorf("knowledge: embed query: %w", err)
	}
	if len(vec) == 0 {
		return nil, nil
	}
	return r.Chunks.Search(ctx, ChunkQuery{
		DatasetIDs: q.datasetIDs(),
		Vector:     vec,
		Mode:       ModeVector,
		TopK:       q.TopK,
	})
}

// LayerRetriever queries LayerRepo for L0/L1 hits. It activates only
// when Query.Layer is LayerAbstract or LayerOverview; LayerDetail
// queries are routed to chunk-tier retrievers instead.
//
// Embedder is consulted only for vector lanes (ModeVector / ModeHybrid).
type LayerRetriever struct {
	Layers   LayerRepo
	Embedder Embedder
}

// NewLayerRetriever constructs a LayerRetriever; pass a nil embedder to
// disable the vector lane.
func NewLayerRetriever(l LayerRepo, e Embedder) *LayerRetriever {
	return &LayerRetriever{Layers: l, Embedder: e}
}

// Name implements Retriever.
func (r *LayerRetriever) Name() string { return "layer" }

// Recall implements Retriever. Vector lane is skipped when Embedder is
// nil; ModeHybrid degrades gracefully to BM25 in that case.
func (r *LayerRetriever) Recall(ctx context.Context, q Query) ([]Candidate, error) {
	if r == nil || r.Layers == nil {
		return nil, nil
	}
	if q.Layer != LayerAbstract && q.Layer != LayerOverview {
		return nil, nil
	}
	mode := ResolveMode(q.Mode)
	wantVector := (mode == ModeVector || mode == ModeHybrid) && r.Embedder != nil && q.Text != ""
	wantBM25 := mode == ModeBM25 || mode == ModeHybrid || (mode == ModeVector && r.Embedder == nil)

	var vec []float32
	if wantVector {
		v, err := r.Embedder.Embed(ctx, q.Text)
		if err != nil {
			return nil, fmt.Errorf("knowledge: embed query: %w", err)
		}
		vec = v
	}

	lq := LayerQuery{
		DatasetIDs: q.datasetIDs(),
		Layer:      q.Layer,
		Text:       q.Text,
		Vector:     vec,
		TopK:       q.TopK,
	}
	switch {
	case wantVector && wantBM25:
		lq.Mode = ModeHybrid
	case wantVector:
		lq.Mode = ModeVector
	default:
		lq.Mode = ModeBM25
	}
	return r.Layers.Search(ctx, lq)
}

// datasetIDs is set by Service.Search before delegating, carrying the
// resolved dataset id list (one entry for ScopeSingleDataset, every
// known dataset for ScopeAllDatasets). When unset, returns nil so
// repos default to "no scope -> no results" (callers must opt in).
func (q Query) datasetIDs() []string {
	if len(q.resolvedDatasets) > 0 {
		return q.resolvedDatasets
	}
	if q.DatasetID != "" {
		return []string{q.DatasetID}
	}
	return nil
}

// Compile-time assertions; keep Retriever set in lockstep.
var (
	_ Retriever = (*BM25Retriever)(nil)
	_ Retriever = (*VectorRetriever)(nil)
	_ Retriever = (*LayerRetriever)(nil)
	_ Ranker    = (*RRFRanker)(nil)
)
