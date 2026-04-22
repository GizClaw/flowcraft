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

	ReturnRaw bool
	MinScore  float64
}

// SearchResponse holds ranked hits.
type SearchResponse struct {
	Hits []Hit
	Took time.Duration

	RawByRetriever map[string][]Hit
}

// Hit is one ranked document.
type Hit struct {
	Doc      Doc
	Score    float64
	Scores   map[string]float64
	Distance float64
}
