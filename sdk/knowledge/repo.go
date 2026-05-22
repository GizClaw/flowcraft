package knowledge

import "context"

// DocumentRepo persists SourceDocuments. Implementations MUST guarantee:
//   - Put atomically assigns SourceDocument.Version / UpdatedAt and
//     returns the authoritative stored document.
//   - Get returns the most recent Put with Content losslessly preserved
//     (contract guarantee #4).
//   - Delete is idempotent.
//
// Put used to return only error, leaving Service to predict the final
// SourceDocument.Version. That contract is deprecated in favour of the
// repository-authoritative return value and will be removed entirely in
// v0.5.0.
//
// Implementations live in sdk/knowledge/backend/*.
type DocumentRepo interface {
	Put(ctx context.Context, doc SourceDocument) (*SourceDocument, error)
	Get(ctx context.Context, datasetID, name string) (*SourceDocument, error)
	Delete(ctx context.Context, datasetID, name string) error
	List(ctx context.Context, datasetID string) ([]SourceDocument, error)
	ListDatasets(ctx context.Context) ([]string, error)
}

// ChunkQuery is the recall input passed by Retrievers to ChunkRepo.Search.
//
// Empty DatasetIDs means "every dataset" (cross-dataset search). When Mode
// is ModeVector or ModeHybrid, Vector should be supplied; backends that
// cannot satisfy a mode return an empty result without error.
type ChunkQuery struct {
	DatasetIDs []string
	Text       string
	Vector     []float32
	Mode       Mode
	TopK       int
}

// ChunkRepo persists DerivedChunks and supports recall.
//
// Replace MUST be atomic: callers rely on it to eliminate stale chunks
// when a SourceDocument is updated (contract guarantee #5).
type ChunkRepo interface {
	Replace(ctx context.Context, datasetID, docName string, chunks []DerivedChunk) error
	// DeleteByDoc removes all chunks for a document. Missing target
	// documents are idempotent success and MUST NOT return NotFound.
	DeleteByDoc(ctx context.Context, datasetID, docName string) error
	DeleteByDataset(ctx context.Context, datasetID string) error
	Search(ctx context.Context, q ChunkQuery) ([]Candidate, error)
}

// DocLevelSearcher is an OPTIONAL extension that ChunkRepo
// implementations may also satisfy. When supported, the backend can
// answer searches at doc-level granularity (one Candidate per docName,
// scored over the whole document rather than per chunk), avoiding the
// chunks→docID collapse that callers (e.g. eval/beir) would otherwise
// have to implement themselves.
//
// SCOPE: BM25 only. Doc-level vector / hybrid retrieval is a separate
// capability — see [DocVectorSearcher]. Service.SearchDocuments routes
// q.Mode == ModeBM25 here and ModeVector / ModeHybrid to
// DocVectorSearcher; backends that do not implement the requested
// capability surface a clear error rather than silently degrading.
// See #126 for the BM25 rationale, #145 for the vector rationale.
type DocLevelSearcher interface {
	SearchDocs(ctx context.Context, q ChunkQuery) ([]Candidate, error)
}

// DocVectorSearcher is an OPTIONAL extension declaring doc-level vector
// (and hybrid) retrieval as an explicit capability (issue #145).
//
// Pre-fix, ChunkRepo.SearchDocs accepted q.Mode and returned
// errdefs.NotAvailable mid-call when the requested mode was anything
// other than ModeBM25. That mixed two unrelated concerns — "what
// granularity is supported" (DocLevelSearcher) and "what scoring
// modes are supported" — into a runtime error that callers had to
// pattern-match on. This interface separates them: the capability is
// declared at compile time and probed via type assertion in
// [Service.SearchDocuments]. Backends that do not implement it
// surface a clear NotAvailable up front, not a buried mid-search
// error.
//
// Hybrid handling is the implementer's responsibility (BM25 + cosine
// fusion, late-chunking, etc.). The interface accepts the same
// [ChunkQuery] as DocLevelSearcher and is expected to honour
// q.Mode == ModeVector or ModeHybrid.
//
// No in-tree implementation yet — both [FSChunkRepo] and
// [RetrievalChunkRepo] hold per-chunk vectors but no per-doc
// representation. Mean-pool / late-chunking will land via a
// follow-up that adds SearchDocsByVector to one or both repos.
type DocVectorSearcher interface {
	SearchDocsByVector(ctx context.Context, q ChunkQuery) ([]Candidate, error)
}

// ChunkSigReader is an OPTIONAL extension that ChunkRepo
// implementations may also satisfy. When supported,
// [Service.Rebuild] can compare the on-disk [DerivedSig] of an
// existing doc against the current (SourceVer, ChunkerSig,
// EmbedSig) and skip the chunk + embed work when the doc is
// already fresh — honouring the [DerivedSig.IsStale] contract
// documented on Rebuild.
//
// Return (DerivedSig{}, false, nil) when the doc has no chunks
// yet (first ingest). Return an error only for backend faults;
// missing docs are NOT an error.
//
// Backends that do not implement ChunkSigReader force Rebuild
// onto the unconditional re-chunk + re-embed path. Fix landed
// in #152.
type ChunkSigReader interface {
	GetDocSig(ctx context.Context, datasetID, docName string) (DerivedSig, bool, error)
}

// LayerQuery is the recall input for layer-tier searches.
type LayerQuery struct {
	DatasetIDs []string
	Layer      Layer
	Text       string
	Vector     []float32
	Mode       Mode
	TopK       int
}

// LayerRepo persists DerivedLayers and supports layer-scoped recall.
type LayerRepo interface {
	Put(ctx context.Context, layer DerivedLayer) error
	Get(ctx context.Context, datasetID, docName string, layer Layer) (*DerivedLayer, error)
	// DeleteByDoc removes all document-level layers for a document.
	// Missing target documents are idempotent success and MUST NOT
	// return NotFound.
	DeleteByDoc(ctx context.Context, datasetID, docName string) error
	DeleteByDataset(ctx context.Context, datasetID string) error
	Search(ctx context.Context, q LayerQuery) ([]Candidate, error)
}

// Candidate is the per-item recall result returned by ChunkRepo / LayerRepo.
// Source identifies the producing retriever ("bm25" / "vector" / "layer")
// and is consumed by the Ranker for fusion.
type Candidate struct {
	Hit    Hit
	Source string
}
