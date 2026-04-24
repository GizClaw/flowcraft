package knowledge

import "context"

// DocumentRepo persists SourceDocuments. Implementations MUST guarantee:
//   - Put atomically increments SourceDocument.Version.
//   - Get returns the most recent Put with Content losslessly preserved
//     (contract guarantee #4).
//   - Delete is idempotent.
//
// Implementations live in sdk/knowledge/backend/*.
type DocumentRepo interface {
	Put(ctx context.Context, doc SourceDocument) error
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
	DeleteByDoc(ctx context.Context, datasetID, docName string) error
	DeleteByDataset(ctx context.Context, datasetID string) error
	Search(ctx context.Context, q ChunkQuery) ([]Candidate, error)
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
