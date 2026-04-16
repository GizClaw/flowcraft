package memory

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/telemetry"

	otellog "go.opentelemetry.io/otel/log"
)

// HybridStore wraps a LongTermStore with optional embedding and vector search
// capabilities. When embedder and vector are nil it behaves identically to the
// inner store with zero overhead.
type HybridStore struct {
	inner    LongTermStore
	embedder Embedder
	vector   VectorSearcher
}

// HybridOption configures a HybridStore.
type HybridOption func(*HybridStore)

// WithEmbedder attaches an Embedder used for pre-computing query vectors and
// as a fallback when SearchOptions.QueryVector is nil.
func WithEmbedder(e Embedder) HybridOption {
	return func(h *HybridStore) { h.embedder = e }
}

// WithVectorSearcher attaches a vector backend for similarity search.
func WithVectorSearcher(v VectorSearcher) HybridOption {
	return func(h *HybridStore) { h.vector = v }
}

// NewHybridStore creates a HybridStore wrapping inner.
// Panics if inner is nil.
func NewHybridStore(inner LongTermStore, opts ...HybridOption) *HybridStore {
	if inner == nil {
		panic("memory: NewHybridStore called with nil inner store")
	}
	h := &HybridStore{inner: inner}
	for _, o := range opts {
		o(h)
	}
	return h
}

func (h *HybridStore) Save(ctx context.Context, runtimeID string, entry *MemoryEntry) error {
	return h.inner.Save(ctx, runtimeID, entry)
}

func (h *HybridStore) List(ctx context.Context, runtimeID string, opts ListOptions) ([]*MemoryEntry, error) {
	return h.inner.List(ctx, runtimeID, opts)
}

func (h *HybridStore) Update(ctx context.Context, runtimeID string, entry *MemoryEntry) error {
	return h.inner.Update(ctx, runtimeID, entry)
}

func (h *HybridStore) Delete(ctx context.Context, runtimeID, entryID string) error {
	return h.inner.Delete(ctx, runtimeID, entryID)
}

// Search performs BM25 search via the inner store. When a VectorSearcher is
// configured it additionally performs vector similarity search and merges the
// two result sets. If QueryVector is not provided but an Embedder is present,
// the embedding is computed on the fly (this path is skipped when the caller
// pre-computes the vector via EmbedQuery).
func (h *HybridStore) Search(ctx context.Context, runtimeID string, query string, opts SearchOptions) ([]*MemoryEntry, error) {
	bm25, err := h.inner.Search(ctx, runtimeID, query, opts)
	if err != nil {
		return nil, err
	}
	if h.vector == nil {
		return bm25, nil
	}

	vec := opts.QueryVector
	if vec == nil && h.embedder != nil {
		vec, err = h.embedder.Embed(ctx, query)
		if err != nil {
			telemetry.Warn(ctx, "hybrid: embed fallback to bm25-only",
				otellog.String("error", err.Error()))
			return bm25, nil
		}
	}
	if vec == nil {
		return bm25, nil
	}

	vecResults, err := h.vector.SearchByVector(ctx, runtimeID, vec, opts)
	if err != nil {
		telemetry.Warn(ctx, "hybrid: vector search fallback to bm25-only",
			otellog.String("runtime_id", runtimeID),
			otellog.String("error", err.Error()))
		return bm25, nil
	}
	return mergeSearchResults(bm25, vecResults, opts.TopK), nil
}

// EmbedQuery pre-computes a query vector. Returns (nil, nil) when no Embedder
// is configured, which lets callers fall through without special-casing.
func (h *HybridStore) EmbedQuery(ctx context.Context, query string) ([]float32, error) {
	if h.embedder == nil {
		return nil, nil
	}
	return h.embedder.Embed(ctx, query)
}

// mergeSearchResults de-duplicates and merges bm25-first, then vector-unique
// entries, capped to topK.
func mergeSearchResults(bm25, vec []*MemoryEntry, topK int) []*MemoryEntry {
	seen := make(map[string]bool, len(bm25)+len(vec))
	out := make([]*MemoryEntry, 0, len(bm25)+len(vec))
	for _, e := range bm25 {
		if e == nil || seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		out = append(out, e)
	}
	for _, e := range vec {
		if e == nil || seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		out = append(out, e)
	}
	if topK > 0 && len(out) > topK {
		out = out[:topK]
	}
	return out
}
