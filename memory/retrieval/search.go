package retrieval

import (
	"strings"
	"time"
)

// HybridMode selects how Search fuses scores when multiple query signals are
// present and Capabilities.Hybrid is true. Mode selects the fusion algorithm;
// [SearchSignal] names the query/search signals that feed that algorithm.
type HybridMode string

const (
	// HybridDefault uses the backend default; retrieval backends should treat
	// it as HybridRRF unless their package documentation says otherwise.
	HybridDefault HybridMode = ""
	// HybridRRF is rank-based Reciprocal Rank Fusion. HybridOptions.K may be a
	// non-negative number; zero uses the backend/scoring default.
	HybridRRF HybridMode = "rrf"
	// HybridWeighted sums raw per-signal scores as weight * score using
	// HybridOptions.Weights. Missing active signal weights default to 1.0.
	HybridWeighted HybridMode = "weighted"
	// HybridConvex first min-max normalizes each signal's scores, then combines
	// them with non-negative HybridOptions.Weights normalized to sum to one.
	// HybridOptions.Alpha is a convenience for the common bm25+vector case
	// (alpha=bm25, 1-alpha=vector).
	HybridConvex HybridMode = "convex"
)

// SearchSignal names an input query/search signal used by hybrid fusion.
//
// SearchSignal is distinct from [HybridMode]: signals describe which
// per-query evidence lanes are present, while HybridMode chooses how those
// lanes are fused. Signal names do not guarantee matching [Hit.Scores] keys;
// score maps use backend/metric-specific labels such as "cos" or "rrf".
type SearchSignal string

const (
	SearchSignalBM25   SearchSignal = "bm25"
	SearchSignalVector SearchSignal = "vector"
	SearchSignalSparse SearchSignal = "sparse"
)

// HybridOptions configures the selected HybridMode.
type HybridOptions struct {
	// K is the RRF damping constant. Zero uses the scorer default.
	K float64
	// Weights configures weighted and convex fusion by SearchSignal. Missing
	// active signal weights default to 1.0.
	Weights map[SearchSignal]float64
	// Alpha is a convex-fusion convenience for bm25+vector searches: alpha is
	// the BM25 weight and 1-alpha is the vector weight.
	Alpha *float64
}

// SearchRequest is a ranked retrieval over one namespace.
type SearchRequest struct {
	QueryText   string
	QueryVector []float32
	SparseVec   map[string]float32

	Filter Filter
	TopK   int

	HybridMode HybridMode
	// HybridOptions configures the selected HybridMode.
	HybridOptions HybridOptions

	// MinScore drops candidates whose final Hit.Score is below this
	// threshold. Backends MUST only apply MinScore on single-modality
	// scoring paths (BM25, cosine, sparse) where the score scale is
	// stable; hybrid / fused scores live on backend-specific scales and
	// are NOT subject to MinScore.
	MinScore float64
}

// SearchResponse holds ranked hits.
type SearchResponse struct {
	Hits []Hit
	Took time.Duration
}

// Hit is one ranked document.
type Hit struct {
	Doc   Doc
	Score float64
	// Scores contains backend/metric-specific score components. Keys are not
	// required to match [SearchSignal] names.
	Scores   map[string]float64
	Distance float64
}

// SearchSignals returns the non-empty query/search signals in req.
func SearchSignals(req SearchRequest) []SearchSignal {
	signals := make([]SearchSignal, 0, 3)
	if strings.TrimSpace(req.QueryText) != "" {
		signals = append(signals, SearchSignalBM25)
	}
	if len(req.QueryVector) > 0 {
		signals = append(signals, SearchSignalVector)
	}
	if len(req.SparseVec) > 0 {
		signals = append(signals, SearchSignalSparse)
	}
	return signals
}

// HasMultipleSearchSignals reports whether req carries two or more search signals.
func HasMultipleSearchSignals(req SearchRequest) bool {
	return len(SearchSignals(req)) > 1
}
