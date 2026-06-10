package document

import "context"

// ListOptions controls ordered chunk scans within a dataset/document.
type ListOptions struct {
	AfterID ChunkID
	Limit   int
	Layer   *Layer
}

// ChunkStore persists document chunk view records.
type ChunkStore interface {
	PutChunk(ctx context.Context, chunk Chunk) (Chunk, error)
	GetChunk(ctx context.Context, datasetID, documentID string, id ChunkID) (Chunk, bool, error)
	ListChunks(ctx context.Context, datasetID, documentID string, opts ListOptions) ([]Chunk, error)
	// DeleteDocument deletes all chunks for the document across layers.
	DeleteDocument(ctx context.Context, datasetID, documentID string) error
	// DeleteDataset deletes all chunks for the dataset across layers.
	DeleteDataset(ctx context.Context, datasetID string) error
}

func cloneListOptions(opts ListOptions) ListOptions {
	if opts.Layer != nil {
		layer := *opts.Layer
		opts.Layer = &layer
	}
	return opts
}
