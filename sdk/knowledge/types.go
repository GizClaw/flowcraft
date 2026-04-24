// Package knowledge provides a side-effect-free knowledge base library:
// document storage, chunking, tokenization, and BM25 / semantic / hybrid
// retrieval over layered context (L0 abstract, L1 overview, L2 chunks).
//
// Storage operations (Store.AddDocument, AddDocuments) only persist raw
// content and update search indexes; they do not call out to an LLM. To
// derive L0/L1, use the stateless GenerateDocumentContext and
// GenerateDatasetContext helpers and publish results back through the
// FSStore setters and sidecar writers. This keeps scheduling, retries,
// caching, and persistence concerns owned entirely by the caller.
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

// Document represents a knowledge base document.
type Document struct {
	Name     string            `json:"name"`
	Content  string            `json:"content"`
	Abstract string            `json:"abstract,omitempty"` // L0
	Overview string            `json:"overview,omitempty"` // L1
	Metadata map[string]string `json:"metadata,omitempty"`
}

// SearchResult represents a single search hit with its relevance score.
type SearchResult struct {
	Content    string         `json:"content"`
	Score      float64        `json:"score"`
	DocName    string         `json:"doc_name,omitempty"`
	ChunkIndex int            `json:"chunk_index,omitempty"`
	Layer      ContextLayer   `json:"layer"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

// SearchMode chooses the retrieval algorithm.
//
// v0.3.0 final values are explicit strings; legacy callers that pass the
// empty string are normalised to ModeBM25 by Mode.Resolve().
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

// SearchOptions configures a knowledge search query.
type SearchOptions struct {
	TopK      int          `json:"top_k,omitempty"`
	MaxLayer  ContextLayer `json:"max_layer,omitempty"`
	Threshold float64      `json:"threshold,omitempty"`
	Mode      SearchMode   `json:"mode,omitempty"`
}

// ChunkConfig controls document chunking.
type ChunkConfig struct {
	ChunkSize    int `json:"chunk_size,omitempty"`
	ChunkOverlap int `json:"chunk_overlap,omitempty"`
}

// DefaultChunkConfig returns the default chunking configuration.
func DefaultChunkConfig() ChunkConfig {
	return ChunkConfig{ChunkSize: 512, ChunkOverlap: 64}
}

const DefaultThreshold = 0.1

// Chunk represents a segment of a document.
type Chunk struct {
	DocName string `json:"doc_name"`
	Index   int    `json:"index"`
	Content string `json:"content"`
	Offset  int    `json:"offset"`
}
