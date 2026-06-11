package document

import (
	"maps"
	"time"

	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const chunksErrPrefix = "memory/views/document/chunks"

// ChunkID is a stable chunk identifier within a dataset/document.
// Layer is a filter dimension, not part of the GetChunk lookup key.
type ChunkID string

// Layer identifies the chunking recipe that produced a chunk.
type Layer struct {
	Name               string
	Version            string
	TransformSignature string
}

// Validate checks that the layer carries a stable recipe identity.
func (l Layer) Validate() error {
	if l.Name == "" {
		return errdefs.Validationf("%s: layer name is required", chunksErrPrefix)
	}
	if l.Version == "" {
		return errdefs.Validationf("%s: layer version is required", chunksErrPrefix)
	}
	return nil
}

// Chunk is one text span derived directly from a canonical document.
//
// Metadata must be JSON-compatible when persisted by a store.
type Chunk struct {
	ID         ChunkID
	Scope      views.Scope
	DocumentID string
	Layer      Layer
	Ordinal    int
	Span       views.Span
	Text       string
	SourceRef  views.SourceRef
	Signature  views.ViewSignature
	CreatedAt  time.Time
	UpdatedAt  time.Time
	Metadata   map[string]any
}

// Validate checks that the chunk is grounded in canonical document evidence.
func (c Chunk) Validate() error {
	return validateChunk(c)
}

func validateChunk(chunk Chunk) error {
	if chunk.ID == "" {
		return errdefs.Validationf("%s: chunk id is required", chunksErrPrefix)
	}
	if err := chunk.Scope.Validate(); err != nil {
		return errdefs.Validationf("%s: invalid scope: %w", chunksErrPrefix, err)
	}
	if chunk.Scope.DatasetID == "" {
		return errdefs.Validationf("%s: dataset_id is required", chunksErrPrefix)
	}
	if chunk.DocumentID == "" {
		return errdefs.Validationf("%s: document_id is required", chunksErrPrefix)
	}
	if err := chunk.Layer.Validate(); err != nil {
		return err
	}
	if chunk.Ordinal < 0 {
		return errdefs.Validationf("%s: ordinal must be non-negative", chunksErrPrefix)
	}
	if err := chunk.Span.Validate(); err != nil {
		return err
	}
	if chunk.Text == "" {
		return errdefs.Validationf("%s: text is required", chunksErrPrefix)
	}
	if err := validateChunkSourceRef(chunk); err != nil {
		return err
	}
	if err := validateChunkSignature(chunk); err != nil {
		return err
	}
	return nil
}

func validateChunkSourceRef(chunk Chunk) error {
	if err := chunk.SourceRef.Validate(); err != nil {
		return err
	}
	if chunk.SourceRef.Kind != views.SourceDocument {
		return errdefs.Validationf("%s: source_ref must reference a document", chunksErrPrefix)
	}
	if chunk.SourceRef.Document == nil {
		return errdefs.Validationf("%s: source_ref document payload is required", chunksErrPrefix)
	}
	ref := chunk.SourceRef.Document
	if ref.DatasetID != chunk.Scope.DatasetID {
		return errdefs.Validationf("%s: source_ref dataset_id %q does not match chunk scope dataset_id %q", chunksErrPrefix, ref.DatasetID, chunk.Scope.DatasetID)
	}
	if ref.DocumentID != chunk.DocumentID {
		return errdefs.Validationf("%s: source_ref document_id %q does not match chunk document_id %q", chunksErrPrefix, ref.DocumentID, chunk.DocumentID)
	}
	if ref.Span == nil || *ref.Span != chunk.Span {
		return errdefs.Validationf("%s: source_ref span must match chunk span", chunksErrPrefix)
	}
	return nil
}

func validateChunkSignature(chunk Chunk) error {
	if len(chunk.Signature.SourceRevisions) == 0 {
		return errdefs.Validationf("%s: document source revisions are required", chunksErrPrefix)
	}
	for i, rev := range chunk.Signature.SourceRevisions {
		if rev.Kind != views.SourceDocument {
			return errdefs.Validationf("%s: source revisions[%d] must reference documents", chunksErrPrefix, i)
		}
	}
	if len(chunk.Signature.UpstreamViewRefs) > 0 {
		return errdefs.Validationf("%s: upstream view refs are not part of document chunk lineage", chunksErrPrefix)
	}
	if err := chunk.Signature.Validate(); err != nil {
		return err
	}
	return nil
}

func sameLayer(a, b Layer) bool {
	return a == b
}

func cloneChunk(chunk Chunk) Chunk {
	chunk.SourceRef = cloneSourceRef(chunk.SourceRef)
	chunk.Signature = cloneViewSignature(chunk.Signature)
	if chunk.Metadata != nil {
		chunk.Metadata = cloneAnyMap(chunk.Metadata)
	}
	return chunk
}

func cloneChunks(chunks []Chunk) []Chunk {
	if chunks == nil {
		return nil
	}
	out := make([]Chunk, len(chunks))
	for i, chunk := range chunks {
		out[i] = cloneChunk(chunk)
	}
	return out
}

func cloneSourceRef(ref views.SourceRef) views.SourceRef {
	if ref.Message != nil {
		msg := *ref.Message
		if msg.Span != nil {
			span := *msg.Span
			msg.Span = &span
		}
		ref.Message = &msg
	}
	if ref.Document != nil {
		doc := *ref.Document
		if doc.Span != nil {
			span := *doc.Span
			doc.Span = &span
		}
		ref.Document = &doc
	}
	return ref
}

func cloneViewSignature(signature views.ViewSignature) views.ViewSignature {
	if signature.SourceRevisions != nil {
		signature.SourceRevisions = append([]views.SourceRevision(nil), signature.SourceRevisions...)
	}
	if signature.UpstreamViewRefs != nil {
		signature.UpstreamViewRefs = append([]views.UpstreamViewRef(nil), signature.UpstreamViewRefs...)
	}
	if signature.DiagnosticSignatures != nil {
		signature.DiagnosticSignatures = maps.Clone(signature.DiagnosticSignatures)
	}
	return signature
}

func cloneAnyMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = cloneAny(v)
	}
	return out
}

func cloneAny(v any) any {
	switch val := v.(type) {
	case map[string]any:
		return cloneAnyMap(val)
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = cloneAny(item)
		}
		return out
	default:
		return v
	}
}
