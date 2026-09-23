package document

import (
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

type RecordKind string

const (
	KindResource RecordKind = "resource"
	KindSection  RecordKind = "section"
	KindChunk    RecordKind = "chunk"
	KindSummary  RecordKind = "summary"
)

// Record is one immutable hierarchy node in an active document build.
type Record struct {
	ID                 string                 `json:"id"`
	Kind               RecordKind             `json:"kind"`
	Level              int                    `json:"level"`
	ParentID           string                 `json:"parent_id,omitempty"`
	Scope              corememory.Scope       `json:"scope"`
	DatasetID          string                 `json:"dataset_id"`
	DocumentID         string                 `json:"document_id"`
	DocumentVersion    uint64                 `json:"document_version"`
	Ordinal            uint64                 `json:"ordinal"`
	Title              string                 `json:"title,omitempty"`
	Content            coremessage.Content    `json:"content"`
	Provenance         []corememory.SourceRef `json:"provenance"`
	SourceDigest       string                 `json:"source_digest"`
	TransformSignature string                 `json:"transform_signature"`
	Metadata           corememory.Metadata    `json:"metadata,omitempty"`
}

// Chunk is retained as a source-compatible alias for callers that only use
// leaf records.
type Chunk = Record

// ReplaceRequest publishes a complete replacement build for one document.
type ReplaceRequest struct {
	Scope           corememory.Scope
	DatasetID       string
	DocumentID      string
	DocumentVersion uint64
	Chunks          []Chunk
}

// ListOptions paginates by the stable (Ordinal, ID) ordering. AfterOrdinal and
// AfterID form an exclusive cursor. A non-positive Limit means no limit.
type ListOptions struct {
	AfterOrdinal uint64
	AfterID      string
	Limit        int
}

func cloneChunk(value Chunk) Chunk {
	value.Content = value.Content.Clone()
	value.Provenance = append([]corememory.SourceRef(nil), value.Provenance...)
	value.Metadata = value.Metadata.Clone()
	return value
}

func cloneChunks(values []Chunk) []Chunk {
	if values == nil {
		return nil
	}
	cloned := make([]Chunk, len(values))
	for index, value := range values {
		cloned[index] = cloneChunk(value)
	}
	return cloned
}
