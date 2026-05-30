package retrieval

import "time"

// HybridMode hints how a hybrid-capable backend should fuse scores.
type HybridMode string

const (
	HybridDefault  HybridMode = ""
	HybridRRF      HybridMode = "rrf"
	HybridWeighted HybridMode = "weighted"
	HybridConvex   HybridMode = "convex"
)

// SearchRequest is a ranked retrieval over one namespace.
type SearchRequest struct {
	QueryText   string
	QueryVector []float32
	SparseVec   map[string]float32

	Filter Filter
	TopK   int

	HybridMode  HybridMode
	HybridParam map[string]any

	// Debug controls how much execution detail backends should attach to
	// SearchResponse.Execution. Zero value disables it.
	Debug SearchDebug

	// MinScore drops candidates whose final Hit.Score is below this
	// threshold. Backends MUST only apply MinScore on single-modality
	// scoring paths (BM25, cosine, sparse) where the score scale is
	// stable; hybrid / fused scores live on backend-specific scales and
	// are NOT subject to MinScore — use pipeline.ScoreThreshold there.
	MinScore float64
}

// SearchResponse holds ranked hits.
type SearchResponse struct {
	Hits []Hit
	Took time.Duration

	// Execution is the structured explanation of how this response was
	// produced. Populated when SearchRequest.Debug requests it; otherwise nil.
	Execution *SearchExecution
}

// Hit is one ranked document.
type Hit struct {
	Doc      Doc
	Score    float64
	Scores   map[string]float64
	Distance float64
}
