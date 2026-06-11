package document

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/views"
)

// ListOptions controls ordered chunk scans within a dataset/document.
type ListOptions struct {
	AfterID ChunkID
	Limit   int
	Layer   *Layer
	Scope   *views.Scope
}

// ChunkStore persists document chunk view records.
type ChunkStore interface {
	PutChunk(ctx context.Context, chunk Chunk) (Chunk, error)
	GetChunk(ctx context.Context, scope views.Scope, documentID string, id ChunkID) (Chunk, bool, error)
	ListChunks(ctx context.Context, documentID string, opts ListOptions) ([]Chunk, error)
	// DeleteDocument deletes all chunks for the document across layers.
	DeleteDocument(ctx context.Context, scope views.Scope, documentID string) error
	// DeleteDataset deletes all chunks for the dataset across layers.
	DeleteDataset(ctx context.Context, scope views.Scope) error
}

func cloneListOptions(opts ListOptions) ListOptions {
	if opts.Layer != nil {
		layer := *opts.Layer
		opts.Layer = &layer
	}
	if opts.Scope != nil {
		scope := *opts.Scope
		opts.Scope = &scope
	}
	return opts
}
