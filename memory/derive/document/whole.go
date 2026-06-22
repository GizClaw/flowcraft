// Package document provides document derivation implementations.
package document

import (
	"context"
	"strconv"
	"strings"

	"github.com/GizClaw/flowcraft/memory/derive"
	"github.com/GizClaw/flowcraft/memory/views"
	viewdocument "github.com/GizClaw/flowcraft/memory/views/document"
)

// WholeDocumentChunker is the deterministic default document chunker. It emits
// the whole document as a single chunk and skips blank documents.
type WholeDocumentChunker struct {
	Layer              viewdocument.Layer
	TransformSignature string
}

func (c WholeDocumentChunker) ChunkDocument(_ context.Context, input derive.DocumentChunkInput) ([]viewdocument.Chunk, error) {
	if strings.TrimSpace(input.Document.Content) == "" {
		return nil, nil
	}
	layer := c.Layer
	if layer.Name == "" {
		layer.Name = "whole_document"
	}
	if layer.Version == "" {
		layer.Version = "v1"
	}
	transformSignature := c.TransformSignature
	if transformSignature == "" {
		transformSignature = layer.TransformSignature
	}
	if transformSignature == "" {
		transformSignature = "whole_document:v1"
	}
	if layer.TransformSignature == "" {
		layer.TransformSignature = transformSignature
	}

	doc := input.Document
	span := views.Span{Start: 0, End: len(doc.Content)}
	ref := views.SourceRef{
		Kind: views.SourceDocument,
		Document: &views.DocumentSourceRef{
			DatasetID:   doc.DatasetID,
			DocumentID:  doc.ID,
			Version:     strconv.FormatUint(doc.Version, 10),
			ContentHash: doc.ContentHash,
			Span:        &span,
		},
	}
	return []viewdocument.Chunk{{
		ID:         "whole",
		Scope:      input.Scope,
		DocumentID: doc.ID,
		Layer:      layer,
		Ordinal:    0,
		Span:       span,
		Text:       doc.Content,
		SourceRef:  ref,
		Signature: views.ViewSignature{
			ViewID: input.View.ID,
			SourceRevisions: []views.SourceRevision{{
				Kind:        views.SourceDocument,
				SourceKey:   ref.StableKey(),
				Revision:    strconv.FormatUint(doc.Version, 10),
				ContentHash: doc.ContentHash,
				ObservedAt:  doc.UpdatedAt,
			}},
			TransformSignature: transformSignature,
		},
	}}, nil
}
