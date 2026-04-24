// Package knowledge implements the v0.3.0 layered knowledge base:
// document storage, chunking, tokenization, and BM25 / vector / hybrid
// retrieval over three context layers (L0 abstract, L1 overview,
// L2 chunk detail).
//
// Architecture
//
//   - Service       (this package): orchestrates DocumentRepo / ChunkRepo /
//     LayerRepo, normalises Query and stamps DerivedSig
//     so callers see a single coherent contract.
//   - factory       (sdk/knowledge/factory): wires Service against either
//     filesystem-backed (factory.NewLocal) or
//     retrieval.Index-backed (factory.NewRetrieval) repositories.
//   - SearchEngine  (this package): runs Retrievers in parallel, fuses with
//     a Ranker (RRF by default).
//   - EventReloader (this package): debounces ChangeEvents and triggers
//     Service.Rebuild with the smallest possible scope.
//
// L0/L1 derivation (GenerateDocumentContext / GenerateDatasetContext) is
// kept external to Service so callers own scheduling, retry and
// persistence policy.
//
// Migration: every v0.2.x symbol survives in deprecated.go (tagged
// // Deprecated:) until v0.3.0; consult deprecated.go for the full
// new-name index.
package knowledge

// ContextLayer indicates the granularity of a search result.
//
// v0.3.0 will rename ContextLayer -> Layer; the alias declared in
// model.go (type Layer = ContextLayer) lets new code adopt the final name
// today without breaking existing callers.
type ContextLayer string

const (
	LayerAbstract ContextLayer = "L0" // ~100 token one-sentence summary
	LayerOverview ContextLayer = "L1" // ~1k token structured overview
	LayerDetail   ContextLayer = "L2" // full chunk content
)

// SearchMode chooses the retrieval algorithm.
//
// v0.3.0 final values are explicit strings; legacy callers that pass the
// empty string are normalised to ModeBM25 by ResolveMode().
//
// Deprecated names:
//   - ModeSemantic remains for backwards compatibility; new code should
//     use ModeVector. They are recognised as equivalent at the Service
//     boundary starting in v0.2.x and ModeSemantic is removed in v0.3.0.
type SearchMode string

const (
	ModeBM25     SearchMode = "bm25"
	ModeVector   SearchMode = "vector"
	ModeSemantic SearchMode = "semantic" // Deprecated: use ModeVector.
	ModeHybrid   SearchMode = "hybrid"
)

// ChunkConfig controls document chunking.
type ChunkConfig struct {
	ChunkSize    int `json:"chunk_size,omitempty"`
	ChunkOverlap int `json:"chunk_overlap,omitempty"`
}

// DefaultChunkConfig returns the default chunking configuration.
func DefaultChunkConfig() ChunkConfig {
	return ChunkConfig{ChunkSize: 512, ChunkOverlap: 64}
}

// DefaultThreshold is the BM25-scale relevance floor used by legacy
// search paths. Service-driven Search uses Query.Threshold instead.
const DefaultThreshold = 0.1
