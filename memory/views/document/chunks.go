package document

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const (
	// DefaultChunksID is the descriptor ID used by NewChunks unless overridden.
	DefaultChunksID views.ID = "document-chunks"

	// DefaultChunksVersion is the descriptor version used by NewChunks unless overridden.
	DefaultChunksVersion = "v1"
)

// Chunks is a lightweight facade for the document chunk semantic view contract.
type Chunks struct {
	store   ChunkStore
	id      views.ID
	version string
}

var _ views.View = (*Chunks)(nil)

// NewChunks creates a document chunk view backed by store.
func NewChunks(store ChunkStore, opts ...Option) *Chunks {
	chunks := &Chunks{
		store:   store,
		id:      DefaultChunksID,
		version: DefaultChunksVersion,
	}
	for _, opt := range opts {
		if opt != nil {
			opt.applyChunks(chunks)
		}
	}
	return chunks
}

// Descriptor declares the Chunks view identity.
func (c *Chunks) Descriptor() views.Descriptor {
	return views.Descriptor{
		ID:      c.id,
		Kind:    views.KindDocumentChunks,
		Version: c.version,
	}
}

// PutChunk stores or replaces a document chunk.
func (c *Chunks) PutChunk(ctx context.Context, chunk Chunk) (Chunk, error) {
	if c.store == nil {
		return Chunk{}, errdefs.Validationf("%s: store is required", chunksErrPrefix)
	}
	if err := validateChunk(chunk); err != nil {
		return Chunk{}, err
	}
	stored, err := c.store.PutChunk(ctx, cloneChunk(chunk))
	if err != nil {
		return Chunk{}, err
	}
	return cloneChunk(stored), nil
}

// GetChunk returns one chunk by dataset, document, and chunk id.
func (c *Chunks) GetChunk(ctx context.Context, datasetID, documentID string, id ChunkID) (Chunk, bool, error) {
	if c.store == nil {
		return Chunk{}, false, errdefs.Validationf("%s: store is required", chunksErrPrefix)
	}
	chunk, ok, err := c.store.GetChunk(ctx, datasetID, documentID, id)
	if err != nil || !ok {
		return Chunk{}, ok, err
	}
	return cloneChunk(chunk), true, nil
}

// ListChunks returns chunks ordered by the backing store contract.
func (c *Chunks) ListChunks(ctx context.Context, datasetID, documentID string, opts ListOptions) ([]Chunk, error) {
	if c.store == nil {
		return nil, errdefs.Validationf("%s: store is required", chunksErrPrefix)
	}
	chunks, err := c.store.ListChunks(ctx, datasetID, documentID, cloneListOptions(opts))
	if err != nil {
		return nil, err
	}
	return cloneChunks(chunks), nil
}

// DeleteDocument removes all chunks for a canonical document.
func (c *Chunks) DeleteDocument(ctx context.Context, datasetID, documentID string) error {
	if c.store == nil {
		return errdefs.Validationf("%s: store is required", chunksErrPrefix)
	}
	return c.store.DeleteDocument(ctx, datasetID, documentID)
}

// DeleteDataset removes all chunks for a canonical dataset.
func (c *Chunks) DeleteDataset(ctx context.Context, datasetID string) error {
	if c.store == nil {
		return errdefs.Validationf("%s: store is required", chunksErrPrefix)
	}
	return c.store.DeleteDataset(ctx, datasetID)
}
