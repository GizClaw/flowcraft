package recall

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/GizClaw/flowcraft/sdk/retrieval/pipeline"
)

// RetrievalStore is the canonical [Store] implementation
// ( Phase 3). It delegates persistence and search to a
// [retrieval.Index] and uses [pipeline.LTM] for hybrid recall.
//
// Namespace layout matches [NamespaceFor]:
//
//	ltm_<runtime>__u_<user>     when scope.UserID != ""
//	ltm_<runtime>__global       otherwise
//
// Per-category recall is achieved via metadata Filter `category=<cat>`,
// not separate namespaces. This keeps the data layout of the high-level
// [Memory] facade and the low-level [Store] in lockstep.
type RetrievalStore struct {
	idx      retrieval.Index
	pipeline *pipeline.Pipeline
	embedder Embedder
	now      func() time.Time
}

// RetrievalStoreOption configures a [RetrievalStore].
type RetrievalStoreOption func(*RetrievalStore)

// WithRetrievalEmbedder enables vector lanes by embedding entries on Save and
// queries on Search. When nil, the store is BM25-only.
func WithRetrievalEmbedder(e Embedder) RetrievalStoreOption {
	return func(s *RetrievalStore) { s.embedder = e }
}

// WithRetrievalPipeline overrides the default [pipeline.LTM].
func WithRetrievalPipeline(p *pipeline.Pipeline) RetrievalStoreOption {
	return func(s *RetrievalStore) { s.pipeline = p }
}

// NewRetrievalStore wires a [Store] to a [retrieval.Index].
// The store is safe for concurrent use and does not own idx (caller closes).
func NewRetrievalStore(idx retrieval.Index, opts ...RetrievalStoreOption) *RetrievalStore {
	s := &RetrievalStore{idx: idx, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	if s.pipeline == nil {
		s.pipeline = pipeline.LTM(asEmbeddingEmbedder(s.embedder))
	}
	return s
}

// Index exposes the underlying [retrieval.Index] for callers who need to drop
// down to retrieval-native APIs (List, Iterate, Snapshot) without going
// through the [Store] facade.
func (s *RetrievalStore) Index() retrieval.Index { return s.idx }

// Save implements [Store].
func (s *RetrievalStore) Save(ctx context.Context, runtimeID string, entry *Entry) error {
	if entry == nil {
		return fmt.Errorf("memory: entry is nil")
	}
	if entry.ID == "" {
		entry.ID = newRetrievalEntryID()
	}
	now := s.now()
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = now
	}
	if entry.UpdatedAt.IsZero() {
		entry.UpdatedAt = now
	}
	if entry.Source.RuntimeID == "" {
		entry.Source.RuntimeID = runtimeID
	}
	if entry.Scope.RuntimeID == "" {
		entry.Scope.RuntimeID = runtimeID
	}
	doc, err := s.encodeDoc(ctx, entry)
	if err != nil {
		return err
	}
	return s.idx.Upsert(ctx, namespaceForScope(runtimeID, entry.Scope), []retrieval.Doc{doc})
}

// Update implements [Store]. Treated as Save (Doc IDs are stable).
func (s *RetrievalStore) Update(ctx context.Context, runtimeID string, entry *Entry) error {
	if entry == nil || entry.ID == "" {
		return fmt.Errorf("memory: update requires entry with ID")
	}
	entry.UpdatedAt = s.now()
	if entry.Scope.RuntimeID == "" {
		entry.Scope.RuntimeID = runtimeID
	}
	doc, err := s.encodeDoc(ctx, entry)
	if err != nil {
		return err
	}
	return s.idx.Upsert(ctx, namespaceForScope(runtimeID, entry.Scope), []retrieval.Doc{doc})
}

// Delete implements [Store]. Best-effort against the global namespace
// only — for known scopes prefer [DeleteScoped].
func (s *RetrievalStore) Delete(ctx context.Context, runtimeID, entryID string) error {
	ns := namespaceForScope(runtimeID, Scope{RuntimeID: runtimeID})
	return s.idx.Delete(ctx, ns, []string{entryID})
}

// DeleteScoped removes an entry from the namespace its scope hashes to.
// Prefer this over Delete when the scope is known (zero fanout).
func (s *RetrievalStore) DeleteScoped(ctx context.Context, runtimeID string, scope Scope, entryID string) error {
	return s.idx.Delete(ctx, namespaceForScope(runtimeID, scope), []string{entryID})
}

// List implements [Store].
func (s *RetrievalStore) List(ctx context.Context, runtimeID string, opts ListOptions) ([]*Entry, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	scope := scopeWithRuntime(opts.Scope, runtimeID)
	namespaces := namespacesForScope(runtimeID, scope)

	var all []*Entry
	for _, ns := range namespaces {
		page, err := s.idx.List(ctx, ns, retrieval.ListRequest{
			PageSize: limit * 2,
			Filter:   categoryFilter(opts.Category),
		})
		if err != nil {
			return nil, err
		}
		for _, d := range page.Items {
			e := docToEntry(d)
			if opts.Category != "" && e.Category != opts.Category {
				continue
			}
			if !EntryMatchesScope(e, scope) {
				continue
			}
			all = append(all, e)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].UpdatedAt.After(all[j].UpdatedAt) })
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// Search implements [Store] via [pipeline.LTM].
func (s *RetrievalStore) Search(ctx context.Context, runtimeID, query string, opts SearchOptions) ([]*Entry, error) {
	if query == "" {
		return nil, nil
	}
	topK := opts.TopK
	if topK <= 0 {
		topK = 10
	}
	scope := scopeWithRuntime(opts.Scope, runtimeID)
	namespaces := namespacesForScope(runtimeID, scope)

	var qVec []float32 = opts.QueryVector
	if qVec == nil && s.embedder != nil {
		v, err := s.embedder.Embed(ctx, query)
		if err == nil {
			qVec = v
		}
	}

	var all []*Entry
	for _, ns := range namespaces {
		req := retrieval.SearchRequest{
			QueryText:   query,
			QueryVector: qVec,
			TopK:        topK,
			Filter:      categoryFilter(opts.Category),
		}
		resp, err := s.pipeline.Run(ctx, s.idx, ns, req)
		if err != nil {
			return nil, err
		}
		for _, h := range resp.Hits {
			if opts.Threshold > 0 && h.Score < opts.Threshold {
				continue
			}
			e := docToEntry(h.Doc)
			if opts.Category != "" && e.Category != opts.Category {
				continue
			}
			if !EntryMatchesScope(e, scope) {
				continue
			}
			all = append(all, e)
		}
	}
	if len(all) > topK {
		all = all[:topK]
	}
	return all, nil
}

// SearchByVector satisfies the optional [VectorSearcher] interface.
func (s *RetrievalStore) SearchByVector(ctx context.Context, runtimeID string, vec []float32, opts SearchOptions) ([]*Entry, error) {
	if len(vec) == 0 {
		return nil, nil
	}
	topK := opts.TopK
	if topK <= 0 {
		topK = 10
	}
	scope := scopeWithRuntime(opts.Scope, runtimeID)
	namespaces := namespacesForScope(runtimeID, scope)

	var all []*Entry
	for _, ns := range namespaces {
		resp, err := s.idx.Search(ctx, ns, retrieval.SearchRequest{
			QueryVector: vec,
			TopK:        topK,
			Filter:      categoryFilter(opts.Category),
		})
		if err != nil {
			return nil, err
		}
		for _, h := range resp.Hits {
			if opts.Threshold > 0 && h.Score < opts.Threshold {
				continue
			}
			e := docToEntry(h.Doc)
			if opts.Category != "" && e.Category != opts.Category {
				continue
			}
			if !EntryMatchesScope(e, scope) {
				continue
			}
			all = append(all, e)
		}
	}
	if len(all) > topK {
		all = all[:topK]
	}
	return all, nil
}

// ----------------------------------------------------------------------------
// helpers
// ----------------------------------------------------------------------------

// namespaceForScope is a thin wrapper around [NamespaceFor] that lets callers
// pass an explicit runtimeID override (used by [Store] methods that
// receive runtimeID separately from the scope).
func namespaceForScope(runtimeID string, s Scope) string {
	if runtimeID != "" {
		s.RuntimeID = runtimeID
	}
	return NamespaceFor(s)
}

// scopeWithRuntime returns a copy of scope with RuntimeID populated when
// missing. Returns nil when scope is nil so legacy "list all" semantics
// (no scope filter) are preserved end-to-end.
func scopeWithRuntime(scope *Scope, runtimeID string) *Scope {
	if scope == nil {
		return nil
	}
	out := *scope
	if out.RuntimeID == "" {
		out.RuntimeID = runtimeID
	}
	return &out
}

// namespacesForScope returns the namespaces that may hold rows matching
// the given query scope. Defaults to the runtime-global namespace when
// scope is nil to preserve legacy "list all" semantics.
func namespacesForScope(runtimeID string, scope *Scope) []string {
	if scope == nil {
		return []string{namespaceForScope(runtimeID, Scope{RuntimeID: runtimeID})}
	}
	out := make([]string, 0, 2)
	seen := make(map[string]struct{}, 2)
	add := func(ns string) {
		if _, ok := seen[ns]; ok {
			return
		}
		seen[ns] = struct{}{}
		out = append(out, ns)
	}
	for _, p := range scope.EffectivePartitions() {
		switch p {
		case PartitionGlobal:
			add(namespaceForScope(runtimeID, Scope{RuntimeID: runtimeID}))
		case PartitionUser:
			if scope.UserID != "" {
				add(namespaceForScope(runtimeID, Scope{RuntimeID: runtimeID, UserID: scope.UserID}))
			}
		}
	}
	if len(out) == 0 {
		add(namespaceForScope(runtimeID, Scope{RuntimeID: runtimeID}))
	}
	return out
}

// categoryFilter pushes category filtering to the index when non-empty.
func categoryFilter(cat Category) retrieval.Filter {
	if cat == "" {
		return retrieval.Filter{}
	}
	return retrieval.Filter{Eq: map[string]any{"category": string(cat)}}
}

func (s *RetrievalStore) encodeDoc(ctx context.Context, e *Entry) (retrieval.Doc, error) {
	doc := EntryToDoc(*e)
	if s.embedder != nil {
		v, err := s.embedder.Embed(ctx, e.Content)
		if err != nil {
			return retrieval.Doc{}, fmt.Errorf("memory: embed entry %s: %w", e.ID, err)
		}
		doc.Vector = v
	}
	return doc, nil
}

// docToEntry returns a heap-allocated copy so the [Store] interface
// (which returns *Entry) and the in-tree DocToEntry helper (which
// returns by value) stay in sync.
func docToEntry(d retrieval.Doc) *Entry {
	e := DocToEntry(d)
	return &e
}

// asEmbeddingEmbedder bridges [Embedder] → [embedding.Embedder].
// Returns nil when input is nil so pipeline.LTM's EmbedQuery becomes a no-op.
func asEmbeddingEmbedder(e Embedder) embedding.Embedder {
	if e == nil {
		return nil
	}
	if ee, ok := e.(embedding.Embedder); ok {
		return ee
	}
	return embedderShim{inner: e}
}

type embedderShim struct{ inner Embedder }

func (s embedderShim) Embed(ctx context.Context, text string) ([]float32, error) {
	return s.inner.Embed(ctx, text)
}

func (s embedderShim) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, err := s.inner.Embed(ctx, t)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// newRetrievalEntryID returns a sortable ID for [RetrievalStore.Save] when the
// caller did not provide one. Uses the package-wide ULID generator.
func newRetrievalEntryID() string {
	return "e_" + NewULID()
}
