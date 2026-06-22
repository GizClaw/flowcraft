package document

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/derive"
	sourcedocument "github.com/GizClaw/flowcraft/memory/sources/document"
	"github.com/GizClaw/flowcraft/memory/views"
	viewdocument "github.com/GizClaw/flowcraft/memory/views/document"
)

func TestWholeDocumentChunkerSkipsWhitespaceDocuments(t *testing.T) {
	chunks, err := (WholeDocumentChunker{}).ChunkDocument(context.Background(), derive.DocumentChunkInput{
		Document: sourcedocument.Document{
			Content: " \n\t ",
		},
	})
	if err != nil {
		t.Fatalf("ChunkDocument() error = %v", err)
	}
	if chunks != nil {
		t.Fatalf("ChunkDocument() chunks = %+v, want nil", chunks)
	}
}

func TestWholeDocumentChunkerSetsLayerSignatureAndObservedAt(t *testing.T) {
	updatedAt := time.Date(2026, 6, 10, 10, 30, 0, 0, time.UTC)
	chunks, err := (WholeDocumentChunker{
		Layer: viewdocument.Layer{
			Name:    "custom",
			Version: "v2",
		},
		TransformSignature: "custom:v2",
	}).ChunkDocument(context.Background(), derive.DocumentChunkInput{
		View:  views.Descriptor{ID: viewdocument.DefaultChunksID},
		Scope: views.Scope{RuntimeID: "runtime-1", DatasetID: "dataset-1"},
		Document: sourcedocument.Document{
			DatasetID:   "dataset-1",
			ID:          "doc-1",
			Content:     "hello world",
			Version:     7,
			ContentHash: "sha256:hello",
			UpdatedAt:   updatedAt,
		},
	})
	if err != nil {
		t.Fatalf("ChunkDocument() error = %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("ChunkDocument() chunks len = %d, want 1", len(chunks))
	}
	chunk := chunks[0]
	if chunk.Layer.TransformSignature != "custom:v2" {
		t.Fatalf("Layer.TransformSignature = %q, want custom:v2", chunk.Layer.TransformSignature)
	}
	if got := chunk.Signature.SourceRevisions[0].ObservedAt; !got.Equal(updatedAt) {
		t.Fatalf("ObservedAt = %v, want %v", got, updatedAt)
	}
	if err := chunk.Validate(); err != nil {
		t.Fatalf("chunk Validate() error = %v", err)
	}
}
