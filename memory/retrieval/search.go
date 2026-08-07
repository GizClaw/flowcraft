package retrieval

import (
	"context"

	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

// Document is one item-level retrieval document. Scope isolation is explicit
// on every write operation and SearchQuery; Payload carries artifact kind,
// provenance, and metadata, never the scope. Index names only projection
// families such as "facts".
type Document struct {
	ID      string
	Text    string
	Vector  []float32
	Payload map[string]any
}

// SearchQuery is the unified search request every backend translates.
// Threshold is a backend-side minimum on the normalized native score; final
// fused thresholds are decided by the retrieval layer.
type SearchQuery struct {
	// Scope is the hard partition every backend must enforce. The memory
	// layer always passes it; OpenSearch translates it into filter clauses.
	Scope     sdkmemory.Scope
	Text      string
	Vector    []float32
	TopK      int
	Threshold float64
	// Filters use the unified operator language
	// (eq/ne/gt/gte/lt/lte/in/nin/contains/icontains plus AND/OR/NOT);
	// backends translate it into their own syntax.
	Filters map[string]any
}

// Hit is one retrieval result.
type Hit struct {
	ID      string
	Score   float64 // [0,1]; higher means more similar; normalization is a backend obligation
	Payload map[string]any
}

// SearchBackend is the item-level retrieval contract for derived memory
// indexes. It is canonical-first: ReplaceAll rebuilds a projection family
// from a complete document set, so backends never need to consume LSM deltas
// or source digests. The existing vector/BM25/entity LSM projections are the
// first implementation; OpenSearch is a future peer.
type SearchBackend interface {
	// Upsert publishes or replaces one document inside one hard scope.
	Upsert(ctx context.Context, index string, scope sdkmemory.Scope, id string, doc Document) error
	// Delete removes one document inside one hard scope; deleting a
	// missing id is a no-op.
	Delete(ctx context.Context, index string, scope sdkmemory.Scope, id string) error
	// ReplaceAll replaces the complete document set of one index family
	// inside one hard scope. Atomicity is per scope partition: a failed
	// call must not leave the scope half-replaced, but separate scopes are
	// independent and require separate calls.
	ReplaceAll(ctx context.Context, index string, scope sdkmemory.Scope, docs []Document) error
	// Search returns hits ordered by descending Score.
	Search(ctx context.Context, index string, query SearchQuery) ([]Hit, error)
}
