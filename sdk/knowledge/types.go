// Package knowledge implements the layered knowledge base: document
// storage, chunking, tokenization, and BM25 / vector / hybrid retrieval
// over three context layers (L0 abstract, L1 overview, L2 chunk detail).
//
// Architecture
//
//   - Service       (this package): orchestrates DocumentRepo / ChunkRepo /
//     LayerRepo, normalises Query and stamps DerivedSig
//     so callers see a single coherent contract.
//   - factory       (sdk/knowledge/factory): wires Service against a
//     retrieval.Index (factory.NewRetrieval). The legacy filesystem
//     wiring factory.NewLocal is deprecated as of v0.4 and slated for
//     removal in v0.5.0; see #134 / docs/migrations/v0.5.0.md.
//   - SearchEngine  (this package): runs Retrievers in parallel, fuses with
//     a Ranker (RRF by default).
//   - EventReloader (this package): debounces ChangeEvents and triggers
//     Service.Rebuild with the smallest possible scope.
//
// L0/L1 derivation (GenerateDocumentContext / GenerateDatasetContext) is
// kept external to Service so callers own scheduling, retry and
// persistence policy.
package knowledge

// Layer indicates the granularity of a search result.
//
// The pre-v0.3.0 name was ContextLayer; that name remains exported as a
// type alias in model.go so callers using either spelling keep compiling.
type Layer string

const (
	LayerAbstract Layer = "L0" // ~100 token one-sentence summary
	LayerOverview Layer = "L1" // ~1k token structured overview
	LayerDetail   Layer = "L2" // full chunk content
)

// SearchMode chooses the retrieval algorithm. Legacy callers that pass
// the empty string are normalised to ModeBM25 by ResolveMode().
type SearchMode string

const (
	ModeBM25   SearchMode = "bm25"
	ModeVector SearchMode = "vector"
	ModeHybrid SearchMode = "hybrid"
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
